// hush-loadtest：hush-server 的压测工具（纯 HTTP / WebSocket 客户端，不依赖服务端内部包）。
//
// 模拟 N 个用户：注册（开发验证码）、上传密钥、建立 WebSocket、按小圈子互加好友，然后按并发 / 速率发送
// Olm 单聊与 Megolm 群消息，接收方逐条 ack。输出吞吐、端到端延迟（发送前打点 → 接收方收到 WS 帧）P50/P95/P99、
// 发送请求耗时、读接口耗时与错误率。消息 body 是不透明字符串（服务端不解码），里面带发送时刻用于计算延迟。
//
// 用法见 server/README.md「压测」一节。目标服务端必须以 --dev 启动并设置 HUSH_DEV_CODE。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type options struct {
	base     string
	code     string
	scenario string
	users    int
	circle   int
	conc     string
	duration time.Duration
	rate     int
	size     int
	groupN   int
	bigN     int
	bigSize  int
	bigCount int
	setupC   int
	dbPath   string
	jsonOut  string
	mdOut    string
	label    string
	ackBatch int
	moments  int
}

func main() {
	var o options
	flag.StringVar(&o.base, "url", "", "目标服务端地址，如 http://127.0.0.1:8095（必填）")
	flag.StringVar(&o.code, "code", "123456", "开发验证码（服务端的 HUSH_DEV_CODE）")
	flag.StringVar(&o.scenario, "scenario", "suite", "场景：suite（全套）| dm（单聊）| group（群消息）| reads（写压下的读延迟）| stall（大群发大消息时的停顿）")
	flag.IntVar(&o.users, "users", 2000, "在线用户数")
	flag.IntVar(&o.circle, "circle", 5, "互为好友的小圈子人数")
	flag.StringVar(&o.conc, "concurrency", "1,16,64,256", "单聊发送并发（逗号分隔，逐档执行）")
	flag.DurationVar(&o.duration, "duration", 15*time.Second, "每一档的持续时间")
	flag.IntVar(&o.rate, "rate", 0, "单聊总发送速率上限（条/秒，0 表示不限：各并发闭环发送）")
	flag.IntVar(&o.size, "size", 256, "单聊消息 body 字节数")
	flag.IntVar(&o.groupN, "group-size", 200, "群消息场景的群人数")
	flag.IntVar(&o.bigN, "big-group-size", 500, "「大群发大消息」场景的群人数")
	flag.IntVar(&o.bigSize, "big-size", 256<<10, "「大群发大消息」的 body 字节数")
	flag.IntVar(&o.bigCount, "big-count", 10, "「大群发大消息」发送条数")
	flag.IntVar(&o.setupC, "setup-concurrency", 32, "注册 / 建连阶段的并发")
	flag.StringVar(&o.dbPath, "db", "", "可选：服务端数据库文件路径（本机压测时用于统计库 + WAL 的增长）")
	flag.StringVar(&o.jsonOut, "json", "", "可选：把结果写成 JSON 文件")
	flag.StringVar(&o.mdOut, "md", "", "可选：把结果写成 Markdown 表格文件")
	flag.StringVar(&o.label, "label", "", "结果标签（写进报告）")
	flag.IntVar(&o.ackBatch, "ack-batch", 1, "接收方每收到多少条 ack 一次（真实客户端逐条 ack）")
	flag.IntVar(&o.moments, "moments", 5, "每个用户预先发布的朋友圈条数（动态流读延迟用）")
	allowDefaultPort := flag.Bool("allow-8080", false, "允许压 8080 端口（默认拒绝：那通常是正在使用的开发服务端）")
	flag.Parse()

	if o.base == "" {
		fatalf("必须用 --url 指定目标服务端（例如 http://127.0.0.1:8095）")
	}
	u, err := url.Parse(o.base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		fatalf("--url 无效: %q", o.base)
	}
	if u.Port() == "8080" && !*allowDefaultPort {
		fatalf("拒绝对 8080 端口压测（会向正在使用的服务端注册大量用户）。确认无误请加 --allow-8080")
	}
	o.base = strings.TrimRight(o.base, "/")
	concs, err := parseInts(o.conc)
	if err != nil {
		fatalf("--concurrency 无效: %v", err)
	}
	if o.circle < 2 || o.users < o.circle {
		fatalf("--users 须不小于 --circle，且 --circle 至少为 2")
	}

	ctx := context.Background()
	rep := &report{Label: o.label, Target: o.base, StartedAt: time.Now().Format(time.RFC3339)}
	w := newWorld(o)
	switch o.scenario {
	case "suite":
		w.setup(ctx, max(o.users, o.bigN))
		rep.Setup = w.setupInfo
		// 先建好两个群：0 号用户因此有几百个好友，读延迟场景里用它量「好友很多时的动态流」
		g := w.makeGroup(ctx, o.groupN)
		big := w.makeGroup(ctx, o.bigN)
		for _, c := range concs {
			rep.add(w.runDM(ctx, c))
		}
		rep.add(w.runReads(ctx, 0))
		rep.add(w.runReads(ctx, 64))
		rep.add(w.runGroup(ctx, g, 1<<10, 1))
		rep.add(w.runGroup(ctx, g, 64<<10, 1))
		rep.add(w.runGroup(ctx, g, 1<<10, 8))
		rep.add(w.runStall(ctx, big, o.bigSize, o.bigCount))
	case "dm":
		w.setup(ctx, o.users)
		rep.Setup = w.setupInfo
		for _, c := range concs {
			rep.add(w.runDM(ctx, c))
		}
	case "group":
		w.setup(ctx, max(o.users, o.groupN))
		rep.Setup = w.setupInfo
		g := w.makeGroup(ctx, o.groupN)
		rep.add(w.runGroup(ctx, g, o.size, 1))
	case "reads":
		w.setup(ctx, o.users)
		rep.Setup = w.setupInfo
		rep.add(w.runReads(ctx, 0))
		rep.add(w.runReads(ctx, 64))
	case "stall":
		w.setup(ctx, max(o.users, o.bigN))
		rep.Setup = w.setupInfo
		big := w.makeGroup(ctx, o.bigN)
		rep.add(w.runStall(ctx, big, o.bigSize, o.bigCount))
	default:
		fatalf("未知场景 %q", o.scenario)
	}
	w.closeAll()
	rep.FinishedAt = time.Now().Format(time.RFC3339)

	fmt.Println()
	fmt.Print(rep.markdown())
	if o.jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(o.jsonOut, b, 0o644); err != nil {
			fatalf("写 JSON 失败: %v", err)
		}
	}
	if o.mdOut != "" {
		if err := os.WriteFile(o.mdOut, []byte(rep.markdown()), 0o644); err != nil {
			fatalf("写 Markdown 失败: %v", err)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hush-loadtest: "+format+"\n", args...)
	os.Exit(2)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n <= 0 {
			return nil, fmt.Errorf("%q 不是正整数", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("为空")
	}
	return out, nil
}
