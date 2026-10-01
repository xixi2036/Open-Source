package ws

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"hush/server/internal/logx"
	"hush/server/internal/store"
)

const (
	readIdleTimeout = 90 * time.Second // 客户端每 25s ping 一次
	writeTimeout    = 15 * time.Second
	drainBatch      = 100
	drainBytes      = 8 << 20 // 每批最多读入的负载字节（100 条 × 1MB 会一次占用 100MB 内存）

	// 连接的实时缓冲：提交钩子直接放进来的、还没写出去的队列项。超出后丢弃并改回从数据库补读（数据都在库里，不会丢）。
	liveMaxItems = 256
	liveMaxBytes = 4 << 20
)

// MaxAckIDs 是单次 ack（HTTP 请求或 WS 帧）的 id 数上限。WS 上超出时分批处理，HTTP 返回 400。
const MaxAckIDs = 1000

// 自定义关闭码。
const (
	CloseReplaced  = 4000 // 同一用户建立了新连接
	CloseLoggedOut = 4001 // 在别处登录 / token 作废
)

var queueEmptyFrame = []byte(`{"t":"queue_empty"}`)
var pongFrame = []byte(`{"t":"pong"}`)

type outFrame struct {
	data   []byte
	close  bool
	code   int
	reason string
}

// liveItem 是提交钩子直接交给连接的一条队列项（已拼好帧；群消息的帧在各接收方之间共享，只读）。
type liveItem struct {
	seq   int64
	id    string
	kind  string
	frame []byte
}

// conn 是一个在线连接：写协程按 seq 顺序发送队列项，读协程处理 ack/ping。
//
// 队列项有两个来源：连接刚建立（或实时缓冲溢出之后）从数据库按 seq 补读；此后由提交钩子直接放进 live 缓冲。
// 两者都严格按 seq 递增，写协程只发送 seq 大于已发送位置的项，所以顺序不乱、同一连接上不重发。
type conn struct {
	hub       *Hub
	userID    string
	tokenHash string
	ws        *websocket.Conn
	cork      *corkConn
	wakeCh    chan struct{}
	ctrl      chan outFrame
	ctx       context.Context
	cancel    context.CancelFunc
	lastRead  atomic.Int64 // 最近一次收到客户端帧的时刻（UnixNano）

	mu        sync.Mutex
	live      []liveItem
	liveBytes int
	overflow  bool // live 丢过东西：需要从数据库补读
}

// offer 由提交钩子调用（写协程里，绝不阻塞）：把新队列项放进实时缓冲并唤醒写协程。
func (c *conn) offer(it liveItem) {
	c.mu.Lock()
	switch {
	case c.overflow:
	case len(c.live) >= liveMaxItems || c.liveBytes+len(it.frame) > liveMaxBytes || len(it.frame) > MaxFrameBytes:
		c.overflow = true
	default:
		c.live = append(c.live, it)
		c.liveBytes += len(it.frame)
	}
	c.mu.Unlock()
	c.wake()
}

func (c *conn) takeLive(buf []liveItem) ([]liveItem, bool) {
	c.mu.Lock()
	buf = append(buf[:0], c.live...)
	for i := range c.live {
		c.live[i] = liveItem{}
	}
	c.live, c.liveBytes = c.live[:0], 0
	ov := c.overflow
	c.overflow = false
	c.mu.Unlock()
	return buf, ov
}

// corkConn 包在被劫持的 TCP 连接外面：写协程「塞住」期间，小帧先攒在内存里，拔掉塞子时一次系统调用写出——
// 积压推送或一次收到多条消息时，几十帧合成一次 write。大帧（群里的大消息）不攒，直接写，避免多拷贝一遍。
type corkConn struct {
	net.Conn
	mu     sync.Mutex
	corked bool
	buf    []byte
}

const (
	corkMaxBuffer = 64 << 10
	corkBigWrite  = 16 << 10
)

func (c *corkConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.corked {
		return c.Conn.Write(p)
	}
	if len(p) >= corkBigWrite || len(c.buf)+len(p) > corkMaxBuffer {
		if err := c.flushLocked(); err != nil {
			return 0, err
		}
		if len(p) >= corkBigWrite {
			return c.Conn.Write(p)
		}
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *corkConn) flushLocked() error {
	if len(c.buf) == 0 {
		return nil
	}
	_, err := c.Conn.Write(c.buf)
	c.buf = c.buf[:0]
	return err
}

func (c *corkConn) setCork(on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.corked = on
	if on {
		return nil
	}
	return c.flushLocked()
}

// hijackWriter 让 websocket.Accept 劫持到的连接变成 corkConn。
type hijackWriter struct {
	http.ResponseWriter
	cork *corkConn
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := h.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	h.cork = &corkConn{Conn: nc}
	return h.cork, bufio.NewReadWriter(brw.Reader, bufio.NewWriter(h.cork)), nil
}

// Serve 升级为 WebSocket 并运行连接直至断开。调用方须已完成鉴权；tokenHash 是本连接所用 token 的哈希。
//
// 鉴权与登记之间可能插入一次重新登录（作废 token → Kick）。为此先记下 epoch 再重新校验 token：
// 校验时 token 已作废 → 以 4001 关闭；校验之后才作废 → Kick 必然在其后更换 epoch，登记时发现变化同样以 4001 关闭。
// 因此 token 已作废的连接既不会成为当前连接，也不会以 4000 挤掉新设备。
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, userID, tokenHash string) {
	h.serving.Add(1)
	defer h.serving.Done()
	if h.closing.Load() {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	epoch := h.currentEpoch(userID)
	valid, err := h.store.TokenValid(r.Context(), userID, tokenHash)
	if err != nil {
		logx.Errorf("[ws] 校验 token 失败 user=%s: %v", userID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hw := &hijackWriter{ResponseWriter: w}
	wsc, err := websocket.Accept(hw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logx.Debugf("[ws] 升级失败 user=%s: %v", userID, err)
		return
	}
	if h.beforeRegister != nil {
		h.beforeRegister(userID)
	}
	wsc.SetReadLimit(1 << 20) // 客户端只发 ping / ack（最多 1000 个 id），1MB 足够
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{
		hub: h, userID: userID, tokenHash: tokenHash, ws: wsc, cork: hw.cork,
		wakeCh: make(chan struct{}, 1),
		ctrl:   make(chan outFrame, 32),
		ctx:    ctx, cancel: cancel,
	}
	c.lastRead.Store(time.Now().UnixNano())
	var old *conn
	registered := false
	if valid && !h.closing.Load() {
		old, registered = h.register(c, epoch)
	}
	if !registered {
		cancel()
		if h.closing.Load() {
			wsc.Close(websocket.StatusCode(CloseGoingAway), "server shutting down")
			return
		}
		wsc.Close(websocket.StatusCode(CloseLoggedOut), "logged_out")
		logx.Debugf("[ws] 拒绝已作废 token 的连接 user=%s", userID)
		return
	}
	if old != nil {
		old.finish(nil, CloseReplaced, "replaced by new connection")
		logx.Debugf("[ws] user=%s 新连接挤掉旧连接", userID)
	}
	logx.Debugf("[ws] 上线 user=%s", userID)
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); c.writeLoop() }()
	c.readLoop()
	cancel()
	h.unregister(c)
	wsc.CloseNow()
	<-writerDone
	logx.Debugf("[ws] 下线 user=%s", userID)
}

// wake 非阻塞地通知写协程有新队列项。
func (c *conn) wake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

// send 非阻塞地排入一个控制帧。
func (c *conn) send(data []byte) {
	select {
	case c.ctrl <- outFrame{data: data}:
	default:
	}
}

// finish 发送可选的最后一帧后以 code 关闭连接。
func (c *conn) finish(data []byte, code int, reason string) {
	select {
	case c.ctrl <- outFrame{data: data, close: true, code: code, reason: reason}:
	default:
		c.cancel()
	}
}

func (c *conn) write(data []byte) error {
	ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// setCork 塞住 / 拔掉塞子；拔掉时把攒下的帧一次写出（带写超时）。
func (c *conn) setCork(on bool) error {
	if c.cork == nil {
		return nil
	}
	if !on {
		c.cork.Conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		defer c.cork.Conn.SetWriteDeadline(time.Time{})
	}
	return c.cork.setCork(on)
}

// drainDB 从数据库按 seq 补读并发送 seq > *last 的全部队列项。
func (c *conn) drainDB(last *int64) error {
	for {
		items, err := c.hub.store.QueueAfter(c.ctx, c.userID, *last, drainBatch, drainBytes)
		if err != nil {
			if c.ctx.Err() == nil {
				logx.Errorf("[ws] 读取队列失败 user=%s: %v", c.userID, err)
			}
			return err
		}
		if len(items) == 0 {
			return nil
		}
		if err := c.setCork(true); err != nil {
			return err
		}
		for _, it := range items {
			*last = it.Seq
			if it.Payload == nil || FrameSize(it.Kind, it.Payload) > MaxFrameBytes {
				// 超限（或正文丢失）的项永远发不出去（客户端也会拒收），留在队列里只会在每次重连时卡住客户端：
				// 直接删除并记录。正常情况下入队前已校验大小，这里是兜底。
				logx.Warnf("[ws] 丢弃无法发送的队列项 user=%s id=%s kind=%s size=%d", c.userID, it.ID, it.Kind, len(it.Payload))
				if _, err := c.hub.store.Ack(c.ctx, c.userID, []string{it.ID}); err != nil {
					logx.Errorf("[ws] 删除无法发送的队列项失败 user=%s: %v", c.userID, err)
				}
				continue
			}
			if err := c.write(Frame(it.Kind, it.Payload)); err != nil {
				c.setCork(false)
				return err
			}
		}
		if err := c.setCork(false); err != nil {
			return err
		}
	}
}

func (c *conn) writeLoop() {
	defer c.cancel()
	var last int64
	sentEmpty := false
	catchUp := true
	var buf []liveItem
	for {
		// 1. 需要时从数据库补读（连接刚建立，或实时缓冲溢出过）。
		if catchUp {
			if err := c.drainDB(&last); err != nil {
				return
			}
			catchUp = false
			// 首次排空后告知积压推送完毕。
			if !sentEmpty {
				if err := c.write(queueEmptyFrame); err != nil {
					return
				}
				sentEmpty = true
			}
		}
		// 2. 发送提交钩子直接交来的队列项（一批帧合成一次写出）。
		var ov bool
		buf, ov = c.takeLive(buf)
		if len(buf) > 0 {
			if err := c.setCork(true); err != nil {
				return
			}
			for i := range buf {
				if buf[i].seq <= last {
					continue // 补读时已经发过
				}
				last = buf[i].seq
				if err := c.write(buf[i].frame); err != nil {
					c.setCork(false)
					return
				}
			}
			if err := c.setCork(false); err != nil {
				return
			}
			for i := range buf {
				buf[i] = liveItem{}
			}
		}
		if ov {
			catchUp = true
			continue
		}
		if len(buf) > 0 {
			continue // 写的时候可能又来了新的
		}
		// 3. 等待新消息或控制帧。
	wait:
		for {
			select {
			case <-c.wakeCh:
				break wait
			case f := <-c.ctrl:
				if f.data != nil {
					if err := c.write(f.data); err != nil {
						return
					}
				}
				if f.close {
					c.ws.Close(websocket.StatusCode(f.code), f.reason)
					return
				}
			case <-c.ctx.Done():
				return
			}
		}
	}
}

type clientFrame struct {
	T   string   `json:"t"`
	IDs []string `json:"ids"`
}

func (c *conn) readLoop() {
	for {
		ctx, cancel := context.WithTimeout(c.ctx, readIdleTimeout)
		_, data, err := c.ws.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		c.lastRead.Store(time.Now().UnixNano())
		var f clientFrame
		if err := json.Unmarshal(data, &f); err != nil {
			logx.Debugf("[ws] 无法解析客户端帧 user=%s: %v", c.userID, err)
			continue
		}
		switch f.T {
		case "ping":
			c.send(pongFrame)
		case "ack":
			if !c.ack(f.IDs) {
				return
			}
		default:
			logx.Debugf("[ws] 未知帧类型 %q user=%s", f.T, c.userID)
		}
	}
}

// ack 分批（每批 MaxAckIDs 个）确认队列项。token 已作废（在别处重新登录）时不删除任何数据，
// 以 4001 关闭连接并返回 false：旧设备不能 ack 掉本该投递给新设备的数据。
func (c *conn) ack(ids []string) bool {
	var total int64
	for len(ids) > 0 {
		batch := ids[:min(len(ids), MaxAckIDs)]
		ids = ids[len(batch):]
		n, err := c.hub.store.AckAs(c.ctx, c.userID, c.tokenHash, batch)
		if errors.Is(err, store.ErrNotFound) {
			logx.Debugf("[ws] token 已作废，拒绝 ack 并断开 user=%s", c.userID)
			c.ws.Close(websocket.StatusCode(CloseLoggedOut), "logged_out")
			return false
		}
		if err != nil {
			if c.ctx.Err() == nil {
				logx.Errorf("[ws] ack 失败 user=%s: %v", c.userID, err)
			}
			return true
		}
		total += n
	}
	logx.Debugf("[ws] ack user=%s 删除 %d 条", c.userID, total)
	return true
}
