package ws

import (
	"bytes"
	"encoding/json"
)

// MaxFrameBytes 是服务端推送的单个 WS 帧（以及 GET /v1/messages 里单条信封）的大小上限。
// 信封 body 上限为 1MB 字符，这里另留 64KB 给信封其余字段与帧外壳。客户端（hushcore）的读帧上限与此一致。
const MaxFrameBytes = 1<<20 + 64<<10

// MarshalNoEscape 序列化为 JSON，但不做 HTML 转义（<、>、& 保持原样，避免密文/字段膨胀为 < 等 6 倍长度）。
func MarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Frame 拼出 {"t":<kind>,"d":<payload>}。payload 必须已是合法 JSON（队列里存的就是），
// 直接拼接字节而不是 json.Marshal(RawMessage)，后者会重新压缩并做 HTML 转义。
func Frame(kind string, payload []byte) []byte {
	out := make([]byte, 0, len(payload)+len(kind)+16)
	out = append(out, `{"t":"`...)
	out = append(out, kind...)
	out = append(out, `","d":`...)
	out = append(out, payload...)
	out = append(out, '}')
	return out
}

// FrameSize 返回 Frame(kind, payload) 的字节数（不实际分配）。
func FrameSize(kind string, payload []byte) int {
	return len(payload) + len(kind) + len(`{"t":"","d":}`)
}
