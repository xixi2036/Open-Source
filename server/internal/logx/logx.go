// Package logx 在标准库 log 之上加了日志级别与异步缓冲输出。
//
// 级别约定：
//   - debug：逐请求、逐条投递 / ack 的流水，以及带社交关系的明细（谁加了谁、谁拉黑谁、谁给哪个群发了消息）。
//     默认不输出——既是隐私面（日志不该成为关系图谱），也是热路径上的开销。
//   - info：启动配置、登录 / 退出、清理结果、管理操作等低频事件。
//   - warn / error：异常。
package logx

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level 是日志级别。
type Level int32

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var level atomic.Int32

func init() { level.Store(int32(Info)) }

// SetLevel 设置输出级别（低于它的不输出）。
func SetLevel(l Level) { level.Store(int32(l)) }

// GetLevel 返回当前级别。
func GetLevel() Level { return Level(level.Load()) }

// Enabled 判断某级别当前是否输出（构造日志参数有开销时先判断）。
func Enabled(l Level) bool { return int32(l) >= level.Load() }

// ParseLevel 解析 debug / info / warn / error。
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return Debug, nil
	case "info", "":
		return Info, nil
	case "warn", "warning":
		return Warn, nil
	case "error":
		return Error, nil
	}
	return Info, fmt.Errorf("未知的日志级别 %q（可用：debug / info / warn / error）", s)
}

// Debugf 输出 debug 级日志。
func Debugf(format string, args ...any) {
	if Enabled(Debug) {
		log.Printf(format, args...)
	}
}

// Infof 输出 info 级日志。
func Infof(format string, args ...any) {
	if Enabled(Info) {
		log.Printf(format, args...)
	}
}

// Warnf 输出 warn 级日志。
func Warnf(format string, args ...any) {
	if Enabled(Warn) {
		log.Printf(format, args...)
	}
}

// Errorf 输出 error 级日志。
func Errorf(format string, args ...any) {
	if Enabled(Error) {
		log.Printf(format, args...)
	}
}

// AsyncWriter 把写入先放进内存缓冲，由后台协程定时刷到底层 Writer：写日志的协程只做一次内存拷贝，
// 不会卡在终端 / 磁盘 I/O 上。缓冲满了就地刷出（不丢日志）。进程退出前必须调用 Close。
type AsyncWriter struct {
	mu   sync.Mutex
	bw   *bufio.Writer
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewAsyncWriter 创建异步缓冲输出：size 是缓冲字节数，every 是刷新间隔。
func NewAsyncWriter(w io.Writer, size int, every time.Duration) *AsyncWriter {
	a := &AsyncWriter{bw: bufio.NewWriterSize(w, size), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				a.Flush()
			case <-a.stop:
				a.Flush()
				return
			}
		}
	}()
	return a
}

func (a *AsyncWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	n, err := a.bw.Write(p)
	a.mu.Unlock()
	return n, err
}

// Flush 把缓冲刷到底层 Writer。
func (a *AsyncWriter) Flush() {
	a.mu.Lock()
	a.bw.Flush()
	a.mu.Unlock()
}

// Close 停止后台协程并刷出剩余内容。之后的写入仍然有效（直接进缓冲，需要自行 Flush）。
func (a *AsyncWriter) Close() {
	a.once.Do(func() { close(a.stop) })
	<-a.done
}
