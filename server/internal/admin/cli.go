package admin

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"hush/server/internal/store"
)

// RunCLI 执行 `hush-server admin <子命令>`，返回进程退出码。
//
//	admin create --data <dir> --username <u> [--role owner|moderator|viewer]   新建管理员
//	admin passwd --data <dir> --username <u>                                   重置密码并解除锁定
//	admin list   --data <dir>                                                  列出管理员
//
// 密码从环境变量 HUSH_ADMIN_PASSWORD 或标准输入（一行）读取，至少 12 位。没有默认账号、默认密码。
func RunCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: hush-server admin <create|passwd|list> --data <dir> [--username <u>] [--role <r>]")
		return 2
	}
	fs := flag.NewFlagSet("admin "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", "./data", "数据目录")
	username := fs.String("username", "", "管理员用户名")
	role := fs.String("role", store.AdminOwner, "角色：owner / moderator / viewer")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if _, err := os.Stat(*dataDir); err != nil {
		fmt.Fprintf(stderr, "数据目录 %s 不存在\n", *dataDir)
		return 1
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "打开数据库失败: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	audit := func(action, target, detail string) store.AuditEntry {
		return store.AuditEntry{AdminID: "cli", AdminName: "(命令行)", Action: action, Target: target, Detail: detail, IP: "local"}
	}
	switch args[0] {
	case "list":
		list, err := st.ListAdmins(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "查询失败: %v\n", err)
			return 1
		}
		for _, a := range list {
			state := "正常"
			if a.Disabled {
				state = "已停用"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", a.Username, a.Role, state)
		}
		return 0
	case "create", "passwd":
		name, err := NormalizeUsername(*username)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if !store.ValidAdminRole(*role) {
			fmt.Fprintln(stderr, "角色须为 owner / moderator / viewer")
			return 2
		}
		password, err := readPassword(stdin, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := CheckPassword(password); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		hash, err := HashPassword(password, DefaultBcryptCost)
		if err != nil {
			fmt.Fprintf(stderr, "生成密码哈希失败: %v\n", err)
			return 1
		}
		if args[0] == "create" {
			a, err := st.CreateAdmin(ctx, name, hash, *role)
			if errors.Is(err, store.ErrConflict) {
				fmt.Fprintf(stderr, "管理员 %s 已存在（重置密码请用 admin passwd）\n", name)
				return 1
			}
			if err != nil {
				fmt.Fprintf(stderr, "创建失败: %v\n", err)
				return 1
			}
			st.AppendAudit(ctx, audit("admin.create", "admin:"+a.ID, fmt.Sprintf(`{"username":%q,"role":%q}`, name, *role)))
			fmt.Fprintf(stdout, "已创建管理员 %s（角色 %s）\n", name, *role)
			return 0
		}
		a, err := st.GetAdminByUsername(ctx, name)
		if err != nil {
			fmt.Fprintf(stderr, "管理员 %s 不存在\n", name)
			return 1
		}
		if err := st.UpdateAdmin(ctx, a.ID, store.AdminUpdate{PasswordHash: &hash}, audit("admin.password", "admin:"+a.ID, `{"cli":true}`)); err != nil {
			fmt.Fprintf(stderr, "重置失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "已重置 %s 的密码，其全部会话已失效\n", name)
		return 0
	}
	fmt.Fprintf(stderr, "未知的子命令 %q（可用：create、passwd、list）\n", args[0])
	return 2
}

// readPassword 从 HUSH_ADMIN_PASSWORD 或标准输入的第一行读取密码。
func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	if p := os.Getenv("HUSH_ADMIN_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprintf(stderr, "请输入密码（至少 %d 位，回车结束；输入会回显，也可改用环境变量 HUSH_ADMIN_PASSWORD）: ", MinPasswordLen)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("未读到密码")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
