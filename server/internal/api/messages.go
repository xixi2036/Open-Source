package api

import (
	"context"
	"encoding/json"
	"errors"
	"hush/server/internal/logx"
	"net/http"
	"time"

	"github.com/google/uuid"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

const (
	maxMessageBody = 1 << 20 // 单条 Olm 密文 body 上限（字符）
	// 群消息 body 上限。群消息是 Megolm 密文文本（图片、语音、文件都走附件接口，消息里只有引用），实际只有几百字节到几 KB；
	// 上限过大只会让一条消息 × 几百个接收方变成一次巨大的扇出。
	maxGroupMessageBody = 256 << 10
	maxMessagesBatch    = 1000     // 单次 POST /v1/messages 条数上限
	maxSendRequest      = 16 << 20 // 发送请求体上限
	maxTokenLen         = 128      // client_id / session_id 上限
	// GET /v1/messages：默认 / 最大条数，以及单页累计负载上限（500 条 × 单条约 1MB 会让一次响应达到数百 MB）。
	defaultFetchLimit = 100
	maxFetchLimit     = 500
	maxFetchBytes     = 8 << 20
)

// validUUID 要求规范的 36 字符 UUID（用户 id、群 id 都由服务端 uuid.NewString 生成）。
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// validToken 校验 client_id / session_id：非空、不超过 128 字符，且只含 base64（标准与 URL 安全）/ UUID 字符集。
func validToken(s string) bool {
	if s == "" || len(s) > maxTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+' || c == '/' || c == '=' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// envelopeFrame 序列化信封（不做 HTML 转义），并检查推送帧不超过 ws.MaxFrameBytes。
func envelopeFrame(env Envelope) ([]byte, bool) {
	payload, err := ws.MarshalNoEscape(env)
	if err != nil {
		return nil, false
	}
	return payload, ws.FrameSize(store.KindEnvelope, payload) <= ws.MaxFrameBytes
}

// Envelope 是服务端→客户端信封（契约 3.3）。
type Envelope struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"` // olm | megolm
	From      string  `json:"from"`
	Type      int     `json:"type"`
	GroupID   *string `json:"group_id"`
	SessionID *string `json:"session_id"`
	Body      string  `json:"body"`
	ServerTS  int64   `json:"server_ts"`
}

type olmResult struct {
	To       string `json:"to"`
	ClientID string `json:"client_id"`
	ID       string `json:"id"`
	ServerTS int64  `json:"server_ts"`
	Error    string `json:"error,omitempty"`
}

// canMessage 判断 from 能否经 Olm 向 to 发消息（也是能否 claim 对方预密钥的判定）：
// 好友或共同在群，且任一方都没有拉黑另一方。返回错误码，空串表示允许。
func (s *Server) canMessage(ctx context.Context, from, to string) (string, error) {
	if from == to {
		return codeBadRequest, nil
	}
	rel, err := s.st.Relation(ctx, from, to)
	if err != nil {
		return "", err
	}
	return relationCode(rel), nil
}

// relationCode 把关系映射为能否发消息的错误码（空串表示允许）。
func relationCode(rel store.Relation) string {
	switch {
	case !rel.Exists:
		return codeNotFound
	case rel.Blocked:
		return codeBlocked
	case rel.Friends || rel.ShareGroup:
		return ""
	default:
		return codeNotFriends
	}
}

// sendAllowed 做发送侧限速：每用户每分钟的条数（单聊按接收方条目计、群消息按条计）与正文字节。
// 超出时已写出 429 并返回 false。客户端把 429 当作临时错误，稍后重试。
func (s *Server) sendAllowed(w http.ResponseWriter, uid string, lim *windowLimiter, n int, bytes int64) bool {
	now := time.Now()
	if !lim.allowN(uid, int64(n), now) || !s.sendBytesLimiter.allowN(uid, bytes, now) {
		logx.Debugf("[ratelimit] user=%s 发送过于频繁", uid)
		w.Header().Set("Retry-After", "5")
		fail(w, codeRateLimited, "发送过于频繁，请稍后再试")
		return false
	}
	return true
}

// emitRejected 给队列已满的接收方留一条合并的「有消息被拒」事件（新事件类型，旧客户端忽略未知事件并照常 ack）。
// 每个接收方的队列里最多保留一条：客户端据此提示用户「有人给你发的消息因为积压太多没能送达」。
func emitRejected(t *store.Tx, users []string, now int64) error {
	for _, uid := range users {
		if err := ws.Emit(t, []string{uid}, "messages_rejected", "messages_rejected", obj{"reason": "queue_full", "at": now}, ws.EmitOpt{Keep: 1}); err != nil {
			return err
		}
	}
	return nil
}

// POST /v1/messages
func (s *Server) handleSendOlm(w http.ResponseWriter, r *http.Request, uid string) {
	var req struct {
		Messages []struct {
			To       string `json:"to"`
			Type     *int   `json:"type"`
			Body     string `json:"body"`
			ClientID string `json:"client_id"`
			Urgent   *bool  `json:"urgent"`
		} `json:"messages"`
	}
	if !decode(w, r, &req, maxSendRequest) {
		return
	}
	if len(req.Messages) == 0 || len(req.Messages) > maxMessagesBatch {
		fail(w, codeBadRequest, "messages 须为 1-1000 条")
		return
	}
	// 第一遍：逐条校验格式，收集需要校验关系的接收方（去重）。
	results := make([]olmResult, len(req.Messages))
	var targets []string
	var bytes int64
	seen := map[string]bool{}
	for i, m := range req.Messages {
		res := olmResult{To: m.To, ClientID: m.ClientID}
		bytes += int64(len(m.Body))
		switch {
		case m.To == "" || m.Type == nil || (*m.Type != 0 && *m.Type != 1) || m.Body == "":
			res.Error = codeBadRequest
		case !validUUID(m.To) || (m.ClientID != "" && !validToken(m.ClientID)):
			res.Error = codeBadRequest
		case len(m.Body) > maxMessageBody:
			res.Error = codeTooLarge
		case m.To == uid:
			res.Error = codeBadRequest // 发给自己
		case !seen[m.To]:
			seen[m.To] = true
			targets = append(targets, m.To)
		}
		results[i] = res
	}
	if !s.sendAllowed(w, uid, s.sendOlmLimiter, len(req.Messages), bytes) {
		return
	}
	// 关系校验：全部接收方一次集合查询（而不是每个接收方一次查询）。
	rels, err := s.st.Relations(r.Context(), uid, targets)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	// 第二遍：填入关系校验结果，挑出要投递的条目。
	var cand []int // 要投递的 req.Messages 下标
	var keys []store.DedupKey
	for i, m := range req.Messages {
		if results[i].Error == "" {
			results[i].Error = relationCode(rels[m.To])
		}
		if results[i].Error == "" {
			cand = append(cand, i)
			if m.ClientID != "" {
				keys = append(keys, store.DedupKey{Target: m.To, ClientID: m.ClientID})
			}
		}
	}
	delivered := 0
	now := store.NowMillis()
	err = s.st.Update(r.Context(), func(t *store.Tx) error {
		delivered = 0
		// 幂等：同一个 (发送方, 接收方, client_id) 24 小时内重复发送，返回第一次的 id 与时间，不再投递。
		hits, err := t.SendDedupLookup(uid, keys, now)
		if err != nil {
			return err
		}
		var items []store.QueueInsert
		var itemRes []int // items[k] 对应 results[itemRes[k]]
		for _, i := range cand {
			m := req.Messages[i]
			res := &results[i]
			res.ID, res.ServerTS, res.Error = "", 0, ""
			if m.ClientID != "" {
				if h, dup := hits[store.DedupKey{Target: m.To, ClientID: m.ClientID}]; dup {
					res.ID, res.ServerTS = h.ID, h.ServerTS
					continue
				}
			}
			id := store.NewID()
			payload, fits := envelopeFrame(Envelope{ID: id, Kind: "olm", From: uid, Type: *m.Type, Body: m.Body, ServerTS: now})
			if !fits {
				res.Error = codeTooLarge
				continue
			}
			res.ID, res.ServerTS = id, now
			if m.ClientID != "" {
				// 同一个请求里重复的 (接收方, client_id) 也只投递一次
				hits[store.DedupKey{Target: m.To, ClientID: m.ClientID}] = store.DedupHit{ID: id, ServerTS: now}
			}
			items = append(items, store.QueueInsert{UserID: m.To, ID: id, Kind: store.KindEnvelope, Payload: payload,
				Urgent: m.Urgent == nil || *m.Urgent, SenderID: uid})
			itemRes = append(itemRes, i)
		}
		verdicts, err := t.EnqueueVerdicts(items, s.queueLimits())
		if err != nil {
			return err
		}
		var recs []store.DedupRecord
		var full []string
		fullSeen := map[string]bool{}
		for k, v := range verdicts {
			res := &results[itemRes[k]]
			if v.OK() {
				delivered++
				if res.ClientID != "" {
					recs = append(recs, store.DedupRecord{DedupKey: store.DedupKey{Target: res.To, ClientID: res.ClientID}, DedupHit: store.DedupHit{ID: res.ID, ServerTS: res.ServerTS}})
				}
				continue
			}
			// 接收方队列已满，或本发送方在对方队列里的子配额已满：拒收（不丢弃已有数据）
			if v == store.RejectedFull && !fullSeen[res.To] {
				fullSeen[res.To] = true
				full = append(full, res.To)
			}
			// 同一请求里后面重复的 (接收方, client_id) 指向了这条被拒的：一并标记
			for _, j := range cand {
				if results[j].ID == res.ID && j != itemRes[k] {
					results[j].ID, results[j].ServerTS, results[j].Error = "", 0, codeTooLarge
				}
			}
			res.ID, res.ServerTS, res.Error = "", 0, codeTooLarge
		}
		if err := t.SendDedupRecord(uid, recs); err != nil {
			return err
		}
		return emitRejected(t, full, now)
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	s.stats.envelopes(delivered, "", time.Now())
	s.metrics.Add("envelopes_enqueued", uint64(delivered))
	logx.Debugf("[deliver] olm from=%s 成功 %d / 共 %d", uid, delivered, len(req.Messages))
	ok(w, obj{"results": results})
}

// POST /v1/groups/{id}/messages → {id, server_ts}
func (s *Server) handleSendMegolm(w http.ResponseWriter, r *http.Request, uid string) {
	gid := r.PathValue("id")
	var req struct {
		SessionID string `json:"session_id"`
		Body      string `json:"body"`
		ClientID  string `json:"client_id"`
		Urgent    *bool  `json:"urgent"`
	}
	if !decode(w, r, &req, maxSendRequest) {
		return
	}
	if req.SessionID == "" || req.Body == "" {
		fail(w, codeBadRequest, "session_id 与 body 必填")
		return
	}
	if len(req.Body) > maxGroupMessageBody {
		fail(w, codeTooLarge, "群消息过大（正文上限 256KB，大内容请走附件）")
		return
	}
	if _, ok := s.requireMember(w, r, gid, uid); !ok {
		return
	}
	if !validToken(req.SessionID) || (req.ClientID != "" && !validToken(req.ClientID)) {
		fail(w, codeBadRequest, "session_id / client_id 须为不超过 128 字符的 base64 或 UUID 字符串")
		return
	}
	if !s.sendAllowed(w, uid, s.sendGroupLimiter, 1, int64(len(req.Body))) {
		return
	}
	id, ts := store.NewID(), store.NowMillis()
	gidCopy, sid := gid, req.SessionID
	payload, fits := envelopeFrame(Envelope{ID: id, Kind: "megolm", From: uid, Type: 0, GroupID: &gidCopy, SessionID: &sid, Body: req.Body, ServerTS: ts})
	if !fits {
		fail(w, codeTooLarge, "消息过大")
		return
	}
	urgent := req.Urgent == nil || *req.Urgent
	var fanout, full int
	duplicate := false
	// 成员身份、接收方名单、幂等检查、扇出都在同一个写事务里：刚被移出群的人发不出去（B3）。
	// 正文只存一份（envelopes 表），各接收方的队列行按 id 引用；写入是十来条多行 INSERT。
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		fanout, full, duplicate = 0, 0, false
		recipients, err := t.GroupSendRecipients(gid, uid)
		if err != nil {
			return err
		}
		var key []store.DedupKey
		if req.ClientID != "" {
			key = []store.DedupKey{{Target: gid, ClientID: req.ClientID}}
			hits, err := t.SendDedupLookup(uid, key, ts)
			if err != nil {
				return err
			}
			if h, dup := hits[key[0]]; dup {
				id, ts, duplicate = h.ID, h.ServerTS, true
				return nil
			}
		}
		items := make([]store.QueueInsert, 0, len(recipients))
		for _, m := range recipients {
			items = append(items, store.QueueInsert{UserID: m, ID: id, Kind: store.KindEnvelope, Payload: payload, Urgent: urgent, SenderID: uid, Shared: true})
		}
		verdicts, err := t.EnqueueVerdicts(items, s.queueLimits())
		if err != nil {
			return err
		}
		// 队列已满的成员被跳过（不影响其他成员），并给他留一条「有消息被拒」事件
		var rejected []string
		for k, v := range verdicts {
			switch v {
			case store.Accepted:
				fanout++
			case store.RejectedFull:
				full++
				rejected = append(rejected, items[k].UserID)
			default:
				full++
			}
		}
		if req.ClientID != "" {
			if err := t.SendDedupRecord(uid, []store.DedupRecord{{DedupKey: key[0], DedupHit: store.DedupHit{ID: id, ServerTS: ts}}}); err != nil {
				return err
			}
		}
		return emitRejected(t, rejected, ts)
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, codeNotFound, "群不存在")
		return
	case errors.Is(err, store.ErrNotMember):
		fail(w, codeForbidden, "你不是该群成员")
		return
	case err != nil:
		internalErr(w, r, err)
		return
	}
	if !duplicate {
		s.stats.envelopes(fanout, gid, time.Now())
		s.metrics.Add("envelopes_enqueued", uint64(fanout))
		logx.Debugf("[deliver] megolm from=%s group=%s 扇出 %d 人（队列已满跳过 %d 人）", uid, gid, fanout, full)
	}
	ok(w, obj{"id": id, "server_ts": ts})
}

// GET /v1/messages?limit=100 → {messages:[信封], events:[事件]}
func (s *Server) handleFetchMessages(w http.ResponseWriter, r *http.Request, uid string) {
	limit, valid := queryInt(r, "limit", defaultFetchLimit)
	if !valid || limit <= 0 {
		fail(w, codeBadRequest, "limit 参数无效")
		return
	}
	limit = min(limit, maxFetchLimit)
	items, err := s.st.QueueAfter(r.Context(), uid, 0, int(limit), maxFetchBytes)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	messages := []json.RawMessage{}
	events := []json.RawMessage{}
	for _, it := range items {
		if it.Kind == store.KindEvent {
			events = append(events, it.Payload)
		} else {
			messages = append(messages, it.Payload)
		}
	}
	ok(w, obj{"messages": messages, "events": events})
}

// POST /v1/messages/ack {"ids":[...]} → {}
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request, uid string) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if len(req.IDs) > ws.MaxAckIDs {
		fail(w, codeBadRequest, "单次最多确认 1000 条")
		return
	}
	n, err := s.st.AckAs(r.Context(), uid, tokenHash(r), req.IDs)
	if errors.Is(err, store.ErrNotFound) {
		// 鉴权之后、ack 之前 token 被重新登录作废：不能删掉发给新设备的数据
		fail(w, codeUnauthorized, "token 无效或已失效")
		return
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	logx.Debugf("[deliver] ack user=%s 删除 %d 条", uid, n)
	ok(w, obj{})
}

// GET /v1/ws
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, uid string) {
	// 长连接：清除 http.Server 的全局读写超时（劫持后的连接会保留这些截止时间），空闲由 ws 包自行检测
	ip := s.clientIP(r)
	switch err := s.hub.Acquire(ip); {
	case errors.Is(err, ws.ErrTooManyConnsPerIP):
		w.Header().Set("Retry-After", "5")
		fail(w, codeRateLimited, "该地址的连接数已达上限")
		return
	case err != nil: // 总连接数已满，或正在停机
		overloaded(w)
		return
	}
	defer s.hub.Release(ip)
	extendDeadlines(w, true, true, 0)
	s.hub.Serve(w, r, uid, tokenHash(r))
}
