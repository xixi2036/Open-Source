package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

const (
	maxMessageBody   = 1 << 20  // 单条密文 body 上限（字符）
	maxMessagesBatch = 1000     // 单次 POST /v1/messages 条数上限
	maxSendRequest   = 16 << 20 // 发送请求体上限
	maxTokenLen      = 128      // client_id / session_id 上限
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
	seen := map[string]bool{}
	for i, m := range req.Messages {
		res := olmResult{To: m.To, ClientID: m.ClientID}
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
	// 关系校验：全部接收方一次集合查询（而不是每个接收方一次查询）。
	rels, err := s.st.Relations(r.Context(), uid, targets)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	// 第二遍：填入关系校验结果并组装信封。
	var items []ws.Item
	var itemRes []int // items[k] 对应 results[itemRes[k]]
	for i, m := range req.Messages {
		res := results[i]
		if res.Error == "" {
			res.Error = relationCode(rels[m.To])
		}
		if res.Error == "" {
			id, ts := store.NewID(), store.NowMillis()
			payload, fits := envelopeFrame(Envelope{ID: id, Kind: "olm", From: uid, Type: *m.Type, Body: m.Body, ServerTS: ts})
			if !fits {
				res.Error = codeTooLarge
			} else {
				res.ID, res.ServerTS = id, ts
				urgent := m.Urgent == nil || *m.Urgent
				items = append(items, ws.Item{UserID: m.To, ID: res.ID, Kind: store.KindEnvelope, Payload: payload, Urgent: urgent})
				itemRes = append(itemRes, i)
			}
		}
		results[i] = res
	}
	accepted, err := s.hub.Deliver(r.Context(), items, s.queueLimits())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	delivered := 0
	for k, okk := range accepted {
		if okk {
			delivered++
			continue
		}
		// 接收方队列已满：拒收（不丢弃已有数据）
		i := itemRes[k]
		log.Printf("[deliver] 接收方 %s 队列已满，拒收", results[i].To)
		results[i].ID, results[i].ServerTS, results[i].Error = "", 0, codeTooLarge
	}
	log.Printf("[deliver] olm from=%s 成功 %d / 共 %d", uid, delivered, len(req.Messages))
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
	if len(req.Body) > maxMessageBody {
		fail(w, codeTooLarge, "消息过大")
		return
	}
	if _, ok := s.requireMember(w, r, gid, uid); !ok {
		return
	}
	if !validToken(req.SessionID) || (req.ClientID != "" && !validToken(req.ClientID)) {
		fail(w, codeBadRequest, "session_id / client_id 须为不超过 128 字符的 base64 或 UUID 字符串")
		return
	}
	// 接收方：除发送者外、且没有拉黑发送者的成员
	recipients, err := s.st.GroupRecipients(r.Context(), gid, uid)
	if err != nil {
		internalErr(w, r, err)
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
	items := make([]ws.Item, 0, len(recipients))
	for _, m := range recipients {
		items = append(items, ws.Item{UserID: m, ID: id, Kind: store.KindEnvelope, Payload: payload, Urgent: urgent})
	}
	accepted, err := s.hub.Deliver(r.Context(), items, s.queueLimits())
	if err != nil {
		internalErr(w, r, err)
		return
	}
	// 队列已满的成员被跳过（不影响其他成员），记日志
	full := 0
	for k, okk := range accepted {
		if !okk {
			full++
			log.Printf("[deliver] 群 %s 成员 %s 队列已满，跳过", gid, items[k].UserID)
		}
	}
	log.Printf("[deliver] megolm from=%s group=%s 扇出 %d 人（队列已满跳过 %d 人）", uid, gid, len(items)-full, full)
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
	log.Printf("[deliver] ack user=%s 删除 %d 条", uid, n)
	ok(w, obj{})
}

// GET /v1/ws
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, uid string) {
	// 长连接：清除 http.Server 的全局读写超时（劫持后的连接会保留这些截止时间），空闲由 ws 包自行检测
	extendDeadlines(w, true, true, 0)
	s.hub.Serve(w, r, uid, tokenHash(r))
}
