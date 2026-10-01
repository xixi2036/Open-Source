package main

import (
	"fmt"
	"sort"
	"strings"
)

// summary 是一组耗时样本的分位数（毫秒）。
type summary struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
	Avg   float64 `json:"avg_ms"`
}

func summarize(ns []int64) summary {
	if len(ns) == 0 {
		return summary{}
	}
	s := append([]int64(nil), ns...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	q := func(p float64) float64 {
		i := int(p*float64(len(s))+0.5) - 1
		i = max(0, min(len(s)-1, i))
		return float64(s[i]) / 1e6
	}
	var sum int64
	for _, v := range s {
		sum += v
	}
	return summary{Count: len(s), P50: q(0.50), P95: q(0.95), P99: q(0.99), Max: float64(s[len(s)-1]) / 1e6, Avg: float64(sum) / float64(len(s)) / 1e6}
}

type readStat struct {
	Path   string `json:"path"`
	Errors int64  `json:"errors"`
	summary
}

// result 是一个场景（或其中一档）的结果。
type result struct {
	Scenario    string  `json:"scenario"`
	Name        string  `json:"name"`
	Concurrency int     `json:"concurrency"`
	Seconds     float64 `json:"seconds"`

	Sent       int64            `json:"sent"`   // 发送请求数
	Errors     int64            `json:"errors"` // 失败的发送
	ErrorKinds map[string]int64 `json:"error_kinds,omitempty"`
	ErrorRate  float64          `json:"error_rate"` // 失败 / 发送
	Throughput float64          `json:"throughput"` // 成功的发送 / 秒

	Expected  int64 `json:"expected"`  // 应投递的信封数
	Delivered int64 `json:"delivered"` // 实际收到（去重后）
	Lost      int64 `json:"lost"`
	Dups      int64 `json:"dups"`

	Send summary `json:"send"` // 发送请求耗时
	E2E  summary `json:"e2e"`  // 端到端：发送前打点 → 接收方收到帧

	Reads         []readStat `json:"reads,omitempty"`
	DBGrowthBytes int64      `json:"db_growth_bytes,omitempty"`
}

func (r *result) finish() {
	if r.Sent > 0 {
		r.ErrorRate = float64(r.Errors) / float64(r.Sent)
	}
	r.Lost = max(0, r.Expected-r.Delivered)
}

type report struct {
	Label      string    `json:"label"`
	Target     string    `json:"target"`
	StartedAt  string    `json:"started_at"`
	FinishedAt string    `json:"finished_at"`
	Setup      setupInfo `json:"setup"`
	Results    []result  `json:"results"`
}

func (r *report) add(res result) { r.Results = append(r.Results, res) }

func ms(v float64) string {
	switch {
	case v == 0:
		return "—"
	case v < 10:
		return fmt.Sprintf("%.2f", v)
	case v < 100:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

func pct(v float64) string { return fmt.Sprintf("%.3f%%", v*100) }

// markdown 把结果渲染成 Markdown 表格。
func (r *report) markdown() string {
	var b strings.Builder
	title := "hush-loadtest 结果"
	if r.Label != "" {
		title += "（" + r.Label + "）"
	}
	fmt.Fprintf(&b, "### %s\n\n", title)
	fmt.Fprintf(&b, "目标 %s；%d 个用户全部在线（%d 人小圈子互为好友）；注册 %.1fs、加好友与发动态 %.1fs、建连 %.1fs。%s → %s\n\n",
		r.Target, r.Setup.Online, r.Setup.Circle, r.Setup.RegisterSec, r.Setup.FriendSec, r.Setup.ConnectSec, r.StartedAt, r.FinishedAt)

	var sends, reads, stalls []result
	for _, x := range r.Results {
		switch x.Scenario {
		case "dm", "group":
			sends = append(sends, x)
		case "reads":
			reads = append(reads, x)
		case "stall":
			stalls = append(stalls, x)
		}
	}
	if len(sends) > 0 {
		b.WriteString("| 场景 | 吞吐（条/秒） | 发送请求 P50 / P95 / P99 (ms) | 端到端 P50 / P95 / P99 / 最大 (ms) | 发送数 | 错误率 | 丢失 | 重复 |\n|---|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, x := range sends {
			fmt.Fprintf(&b, "| %s | %.0f | %s / %s / %s | %s / %s / %s / %s | %d | %s | %d | %d |\n", x.Name, x.Throughput,
				ms(x.Send.P50), ms(x.Send.P95), ms(x.Send.P99), ms(x.E2E.P50), ms(x.E2E.P95), ms(x.E2E.P99), ms(x.E2E.Max), x.Sent, pct(x.ErrorRate), x.Lost, x.Dups)
		}
		b.WriteString("\n")
	}
	if len(reads) > 0 {
		b.WriteString("| 读接口 | 条件 | P50 (ms) | P95 (ms) | P99 (ms) | 最大 (ms) | 次数 | 失败 |\n|---|---|---:|---:|---:|---:|---:|---:|\n")
		for _, x := range reads {
			cond := "空闲"
			if x.Concurrency > 0 {
				cond = fmt.Sprintf("单聊写压 并发 %d（%.0f 条/秒）", x.Concurrency, x.Throughput)
			}
			for _, rs := range x.Reads {
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d | %d |\n", rs.Path, cond, ms(rs.P50), ms(rs.P95), ms(rs.P99), ms(rs.Max), rs.Count, rs.Errors)
			}
		}
		b.WriteString("\n")
	}
	for _, x := range stalls {
		fmt.Fprintf(&b, "**%s**：发送请求 P50 %s ms / 最大 %s ms；全体端到端 P50 %s / P95 %s / 最大 %s ms；错误 %d，丢失 %d，重复 %d",
			x.Name, ms(x.Send.P50), ms(x.Send.Max), ms(x.E2E.P50), ms(x.E2E.P95), ms(x.E2E.Max), x.Errors, x.Lost, x.Dups)
		if x.DBGrowthBytes != 0 {
			fmt.Fprintf(&b, "；库 + WAL 增长 %s", human(int(x.DBGrowthBytes)))
		}
		b.WriteString("。期间其他请求：\n\n| 探针 | P50 (ms) | P95 (ms) | P99 (ms) | 最大停顿 (ms) | 次数 | 失败 |\n|---|---:|---:|---:|---:|---:|---:|\n")
		for _, rs := range x.Reads {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d | %d |\n", rs.Path, ms(rs.P50), ms(rs.P95), ms(rs.P99), ms(rs.Max), rs.Count, rs.Errors)
		}
		b.WriteString("\n")
	}
	var kinds []string
	for _, x := range r.Results {
		for k, n := range x.ErrorKinds {
			kinds = append(kinds, fmt.Sprintf("%s：%s × %d", x.Name, k, n))
		}
	}
	if len(kinds) > 0 {
		sort.Strings(kinds)
		b.WriteString("错误明细：" + strings.Join(kinds, "；") + "\n\n")
	}
	return b.String()
}
