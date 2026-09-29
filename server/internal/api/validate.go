package api

import (
	"net/http"
	"unicode/utf8"
)

// 文本长度上限（按字符计）。
const (
	maxNameLen      = 64   // 昵称、群名
	maxShortText    = 256  // 个性签名、好友申请留言
	maxAnnouncement = 2000 // 群公告
)

// validName 校验昵称 / 群名：1–64 个字符。
func validName(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= maxNameLen
}

// validShortText 校验签名 / 好友申请留言：不超过 256 个字符。
func validShortText(s string) bool {
	return utf8.RuneCountInString(s) <= maxShortText
}

// requireAvatar 校验头像附件：nil 或空串（表示不修改 / 清除）直接通过，否则必须是已上传的附件（404）。
// 失败时已写出错误并返回 false。
func (s *Server) requireAvatar(w http.ResponseWriter, r *http.Request, avatarID *string, notFoundMsg string) bool {
	if avatarID == nil || *avatarID == "" {
		return true
	}
	if _, err := s.st.GetAttachment(r.Context(), *avatarID); err != nil {
		storeErr(w, r, err, notFoundMsg)
		return false
	}
	return true
}
