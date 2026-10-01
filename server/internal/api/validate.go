package api

import "unicode/utf8"

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
