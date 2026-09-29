// Package push 定义离线推送（FCM/APNs）接口。本地部署只提供日志实现。
package push

import "log"

// Pusher 在接收方离线且消息为 urgent 时被调用，用于唤醒客户端。
type Pusher interface {
	Push(userID, reason string)
}

// LogPusher 只把推送意图写入日志。
type LogPusher struct{}

// Push 实现 Pusher。
func (LogPusher) Push(userID, reason string) {
	log.Printf("[push] 用户 %s 离线，需推送唤醒（%s）——未接入 FCM/APNs，仅记录", userID, reason)
}
