// token 子命令：员工专属 API token 的签发 / 列表 / 轮换 / 吊销。
//
//	adapter token <issue|list|rotate|revoke> [-config config.yaml] [-person <wecom_user_id>]
//
// 只打开 SQLite（storage），不启动引擎、不连 NAS、不起 HTTP 服务。
// token 明文只在 issue / rotate 时输出到标准输出一次，系统里不保存（库里只有哈希）。
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/config"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/identity"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/tokenauth"
)

// tokenUsage token 子命令的帮助文本。
const tokenUsage = `用法: adapter token <子命令> [-config config.yaml] [-person <wecom_user_id>]

子命令:
  issue   -person <id>   为员工签发新 token（已有有效 token 时报错，请改用 rotate）
  list                   列出全部 token（不含明文/哈希）
  rotate  -person <id>   轮换 token，旧 token 在 server.token_grace 宽限期后失效
  revoke  -person <id>   吊销该员工全部 token

示例:
  adapter token issue  -config config.yaml -person ZhangSan
  adapter token list   -config config.yaml
  adapter token rotate -config config.yaml -person ZhangSan
`

// tokenMain token 子命令入口。
func tokenMain(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, tokenUsage)
		os.Exit(1)
	}
	cmd := args[0]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(tokenUsage)
		return
	case "issue", "list", "rotate", "revoke":
	default:
		fmt.Fprintf(os.Stderr, "未知 token 子命令 %q\n\n%s", cmd, tokenUsage)
		os.Exit(1)
	}

	fs := flag.NewFlagSet("token "+cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "配置文件路径")
	person := fs.String("person", "", "员工 ID（identities.wecom_user_id）")
	_ = fs.Parse(args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}
	// 只开存储，不启引擎 / 不连 NAS。
	st, err := storage.Open(cfg.Storage.DBFile)
	if err != nil {
		log.Fatalf("存储层初始化失败: %v", err)
	}
	defer st.Close()

	pid := strings.TrimSpace(*person)
	switch cmd {
	case "list":
		if err := tokenList(st); err != nil {
			log.Fatalf("列出 token 失败: %v", err)
		}
	case "issue":
		if err := tokenIssue(cfg, st, pid); err != nil {
			log.Fatalf("%v", err)
		}
	case "rotate":
		if err := tokenRotate(cfg, st, pid, cfg.Server.TokenGrace.D()); err != nil {
			log.Fatalf("%v", err)
		}
	case "revoke":
		if err := tokenRevoke(cfg, st, pid); err != nil {
			log.Fatalf("%v", err)
		}
	}
}

// requirePerson 校验 -person 且确认该员工存在（避免给打错的 ID 签发 token）。
// identities 表可能还没被 daemon 填过（local 模式是惰性加载），这时代码会先
// 把本地映射文件导入 SQLite 再查一次——依然只碰本地文件，不连 NAS、不联网。
func requirePerson(cfg *config.Config, st *storage.Store, pid, action string) (*storage.Identity, error) {
	if pid == "" {
		return nil, fmt.Errorf("%s 需要 -person <wecom_user_id>", action)
	}
	id, err := st.GetIdentityByWeCom(pid)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("查询员工 %q: %w", pid, err)
		}
		// 兜底：source=local 时把映射文件导入 identities 后再查一次。
		if cfg.Identity.Source == "local" || cfg.Identity.Source == "" {
			resolver := identity.NewLocalResolver(cfg.Identity.LocalFile, st)
			id, err = resolver.ResolveWeCom(pid)
		}
		if err != nil || id == nil {
			return nil, fmt.Errorf("identities 里没有员工 %q（检查 -person 是否为其企微 userid）", pid)
		}
	}
	// 姓名是员工根目录名，为空就拼不出根目录，token 发下去也用不了。
	if strings.TrimSpace(id.PersonName) == "" {
		return nil, fmt.Errorf("员工 %q 的 person_name 为空，无法拿到 NAS 根目录；请先补全身份映射", pid)
	}
	return id, nil
}

// tokenIssue 签发新 token，明文只打印一次。
func tokenIssue(cfg *config.Config, st *storage.Store, pid string) error {
	id, err := requirePerson(cfg, st, pid, "issue")
	if err != nil {
		return err
	}
	active, err := st.HasActiveAPIToken(pid)
	if err != nil {
		return fmt.Errorf("检查已有 token: %w", err)
	}
	if active {
		return fmt.Errorf("员工 %q 已有有效 token；需要换新请用: adapter token rotate -person %s", pid, pid)
	}
	token, err := st.IssueAPIToken(pid)
	if err != nil {
		return fmt.Errorf("签发 token: %w", err)
	}
	printNewToken(id, token)
	return nil
}

// tokenRotate 轮换 token：新 token 立即生效，旧 token 在 grace 后失效。
func tokenRotate(cfg *config.Config, st *storage.Store, pid string, grace time.Duration) error {
	id, err := requirePerson(cfg, st, pid, "rotate")
	if err != nil {
		return err
	}
	token, err := st.RotateAPIToken(pid, grace)
	if err != nil {
		return fmt.Errorf("轮换 token: %w", err)
	}
	printNewToken(id, token)
	if grace > 0 {
		fmt.Printf("旧 token 失效时间: %s（宽限期 %s）\n",
			time.Now().Add(grace).Format("2006-01-02 15:04:05"), grace)
	} else {
		fmt.Println("旧 token 已立即失效（宽限期为 0）")
	}
	return nil
}

// tokenRevoke 吊销该员工全部 token。
func tokenRevoke(cfg *config.Config, st *storage.Store, pid string) error {
	if _, err := requirePerson(cfg, st, pid, "revoke"); err != nil {
		return err
	}
	n, err := st.RevokeAPITokens(pid)
	if err != nil {
		return fmt.Errorf("吊销 token: %w", err)
	}
	fmt.Printf("已吊销员工 %s 的 %d 个 token\n", pid, n)
	if n == 0 {
		fmt.Println("（该员工原本没有未吊销的 token）")
	}
	return nil
}

// printNewToken 把明文打到标准输出（仅此一次），提示语打到标准错误，
// 方便 `adapter token issue ... > token.txt` 只拿到明文。
func printNewToken(id *storage.Identity, token string) {
	name := ""
	if id != nil {
		name = id.PersonName
	}
	if name != "" {
		fmt.Fprintf(os.Stderr, "员工: %s（%s）\n", name, id.WeComUserID)
	} else if id != nil {
		fmt.Fprintf(os.Stderr, "员工: %s\n", id.WeComUserID)
	}
	fmt.Println(token)
	fmt.Fprintln(os.Stderr, "⚠️ 请立即交付，系统不保存明文（库里只有哈希）；丢失请用 rotate 重新签发。")
}

// tokenList 表格列出全部 token（不含明文与哈希）。
func tokenList(st *storage.Store) error {
	list, err := st.ListAPITokens()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("（还没有任何 token；可用 adapter token issue -person <id> 签发）")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "前缀\t员工ID\t姓名\t状态\t签发时间\t过期时间\t最近使用")
	now := time.Now().Unix()
	for _, t := range list {
		name := ""
		if id, err := st.GetIdentityByWeCom(t.PersonID); err == nil && id != nil {
			name = id.PersonName
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Display, t.PersonID, orDash(name), tokenStatusText(t, now),
			fmtTime(t.CreatedAt), expireText(t.ExpiresAt), fmtTime(t.LastUsedAt))
	}
	return w.Flush()
}

// tokenStatusText active / revoked / expired（已经过了宽限期的 active token）。
func tokenStatusText(t storage.APIToken, now int64) string {
	switch {
	case t.Status == "revoked":
		return "revoked"
	case t.ExpiresAt > 0 && t.ExpiresAt <= now:
		return "expired"
	default:
		return "active"
	}
}

// fmtTime unix 秒 → 本地时间字符串。
func fmtTime(ts int64) string {
	if ts <= 0 {
		return "—"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// expireText 过期时间；0 表示不过期。
func expireText(ts int64) string {
	if ts <= 0 {
		return "不过期"
	}
	return fmtTime(ts)
}

// orDash 空值显示为破折号。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// ===== 自动签发（daemon 启动时 + 每 10 分钟）=====

// pendingHeader 待交付清单表头。清单是敏感文件（含明文 token），只落本地 secrets/，不进仓库。
var pendingHeader = []string{"签发时间", "姓名", "员工ID", "baseUrl", "token"}

// issuablePerson 待扫描的员工：person_name 非空即有资格持有 token。
type issuablePerson struct {
	PersonID   string
	PersonName string
}

// issuedToken 一条待交付记录（person + 明文，只在内存里短暂存在）。
type issuedToken struct {
	person issuablePerson
	token  string
}

// issuePendingTokens 为「person_name 非空且还没有有效 token」的员工自动签发，
// 并把明文追加写入 cfg.Server.PendingFile。日志里绝不打印 token。
// 返回本次签发的人数。
func issuePendingTokens(st *storage.Store, externalURL, pendingFile string) (int, error) {
	people, err := scanIssuablePeople(st)
	if err != nil {
		return 0, fmt.Errorf("读取员工名单: %w", err)
	}

	var out []issuedToken
	for _, p := range people {
		active, err := st.HasActiveAPIToken(p.PersonID)
		if err != nil {
			return 0, fmt.Errorf("检查员工 %s 的 token: %w", p.PersonID, err)
		}
		if active {
			continue
		}
		token, err := st.IssueAPIToken(p.PersonID)
		if err != nil {
			return 0, fmt.Errorf("为员工 %s 签发 token: %w", p.PersonID, err)
		}
		out = append(out, issuedToken{person: p, token: token})
	}
	if len(out) == 0 {
		return 0, nil
	}
	if err := appendPendingCSV(pendingFile, externalURL, out); err != nil {
		// 明文没写进清单就没人拿得到：立刻吊销，下一轮 HasActiveAPIToken=false 会重新签发。
		for _, it := range out {
			if _, rerr := st.RevokeAPITokens(it.person.PersonID); rerr != nil {
				log.Printf("[警告] 回收未交付的 token 失败（员工 %s）: %v", it.person.PersonID, rerr)
			}
		}
		return 0, fmt.Errorf("写入待交付清单 %s: %w", pendingFile, err)
	}
	return len(out), nil
}

// scanIssuablePeople 只读扫描 identities 里 person_name 非空的员工。
func scanIssuablePeople(st *storage.Store) ([]issuablePerson, error) {
	ids, err := st.ListIdentities()
	if err != nil {
		return nil, err
	}
	var out []issuablePerson
	for _, id := range ids {
		p := issuablePerson{PersonID: strings.TrimSpace(id.WeComUserID), PersonName: strings.TrimSpace(id.PersonName)}
		if p.PersonID == "" || p.PersonName == "" {
			continue
		}
		// 姓名不合法的（含 / 或为 . / ..）不签发：拼出来的根目录会被 tokenauth 拒绝。
		if !tokenauth.ValidPersonName(p.PersonName) {
			log.Printf("[警告] 员工 %s 的姓名 %q 不能作为目录名，跳过签发", p.PersonID, p.PersonName)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// appendPendingCSV 追加写入待交付清单：目录 0700、文件 0600，首次创建写表头。
func appendPendingCSV(path, externalURL string, issued []issuedToken) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	_, statErr := os.Stat(path)
	firstWrite := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开文件: %w", err)
	}
	defer f.Close()
	// 已存在的文件可能权限更宽（手工创建），收紧一次。
	_ = os.Chmod(path, 0o600)

	w := csv.NewWriter(f)
	if firstWrite {
		if err := w.Write(pendingHeader); err != nil {
			return err
		}
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	for _, it := range issued {
		row := []string{now, it.person.PersonName, it.person.PersonID, externalURL, it.token}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Sync()
}
