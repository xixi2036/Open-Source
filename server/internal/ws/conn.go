package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"hush/server/internal/store"
)

const (
	readIdleTimeout = 90 * time.Second // 客户端每 25s ping 一次
	writeTimeout    = 15 * time.Second
	drainBatch      = 100
	drainBytes      = 8 << 20 // 每批最多读入的负载字节（100 条 × 1MB 会一次占用 100MB 内存）
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

// conn 是一个在线连接：写协程负责按 seq 顺序从队列排空并发送，读协程处理 ack/ping。
type conn struct {
	hub       *Hub
	userID    string
	tokenHash string
	ws        *websocket.Conn
	wakeCh    chan struct{}
	ctrl      chan outFrame
	ctx       context.Context
	cancel    context.CancelFunc
}

// Serve 升级为 WebSocket 并运行连接直至断开。调用方须已完成鉴权；tokenHash 是本连接所用 token 的哈希。
//
// 鉴权与登记之间可能插入一次重新登录（作废 token → Kick）。为此先记下 epoch 再重新校验 token：
// 校验时 token 已作废 → 以 4001 关闭；校验之后才作废 → Kick 必然在其后递增 epoch，登记时发现变化同样以 4001 关闭。
// 因此 token 已作废的连接既不会成为当前连接，也不会以 4000 挤掉新设备。
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, userID, tokenHash string) {
	epoch := h.currentEpoch(userID)
	valid, err := h.store.TokenValid(r.Context(), userID, tokenHash)
	if err != nil {
		log.Printf("[ws] 校验 token 失败 user=%s: %v", userID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		log.Printf("[ws] 升级失败 user=%s: %v", userID, err)
		return
	}
	if h.beforeRegister != nil {
		h.beforeRegister(userID)
	}
	wsc.SetReadLimit(1 << 20) // 客户端只发 ping / ack（最多 1000 个 id），1MB 足够
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{
		hub: h, userID: userID, tokenHash: tokenHash, ws: wsc,
		wakeCh: make(chan struct{}, 1),
		ctrl:   make(chan outFrame, 32),
		ctx:    ctx, cancel: cancel,
	}
	var old *conn
	registered := false
	if valid {
		old, registered = h.register(c, epoch)
	}
	if !registered {
		cancel()
		wsc.Close(websocket.StatusCode(CloseLoggedOut), "logged_out")
		log.Printf("[ws] 拒绝已作废 token 的连接 user=%s", userID)
		return
	}
	if old != nil {
		old.finish(nil, CloseReplaced, "replaced by new connection")
		log.Printf("[ws] user=%s 新连接挤掉旧连接", userID)
	}
	log.Printf("[ws] 上线 user=%s remote=%s", userID, r.RemoteAddr)
	go c.writeLoop()
	c.readLoop()
	cancel()
	h.unregister(c)
	wsc.CloseNow()
	log.Printf("[ws] 下线 user=%s", userID)
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

func (c *conn) writeLoop() {
	defer c.cancel()
	var last int64
	sentEmpty := false
	for {
		// 1. 排空 seq > last 的全部队列项。
		for {
			items, err := c.hub.store.QueueAfter(c.ctx, c.userID, last, drainBatch, drainBytes)
			if err != nil {
				if c.ctx.Err() == nil {
					log.Printf("[ws] 读取队列失败 user=%s: %v", c.userID, err)
				}
				return
			}
			for _, it := range items {
				last = it.Seq
				if FrameSize(it.Kind, it.Payload) > MaxFrameBytes {
					// 超限的项永远发不出去（客户端也会拒收），留在队列里只会在每次重连时卡住客户端：
					// 直接删除并记录。正常情况下入队前已校验大小，这里是兜底。
					log.Printf("[ws] 丢弃超限队列项 user=%s id=%s kind=%s size=%d", c.userID, it.ID, it.Kind, len(it.Payload))
					if _, err := c.hub.store.Ack(c.ctx, c.userID, []string{it.ID}); err != nil {
						log.Printf("[ws] 删除超限队列项失败 user=%s: %v", c.userID, err)
					}
					continue
				}
				if err := c.write(Frame(it.Kind, it.Payload)); err != nil {
					return
				}
			}
			if len(items) == 0 {
				break
			}
		}
		// 2. 首次排空后告知积压推送完毕。
		if !sentEmpty {
			if err := c.write(queueEmptyFrame); err != nil {
				return
			}
			sentEmpty = true
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
		var f clientFrame
		if err := json.Unmarshal(data, &f); err != nil {
			log.Printf("[ws] 无法解析客户端帧 user=%s: %v", c.userID, err)
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
			log.Printf("[ws] 未知帧类型 %q user=%s", f.T, c.userID)
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
			log.Printf("[ws] token 已作废，拒绝 ack 并断开 user=%s", c.userID)
			c.ws.Close(websocket.StatusCode(CloseLoggedOut), "logged_out")
			return false
		}
		if err != nil {
			log.Printf("[ws] ack 失败 user=%s: %v", c.userID, err)
			return true
		}
		total += n
	}
	log.Printf("[ws] ack user=%s 删除 %d 条", c.userID, total)
	return true
}
