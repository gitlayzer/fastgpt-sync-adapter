// Package auth 飞书 OAuth 授权 webhook（常驻，与 daemon 同进程）。
// 职责：接收 OAuth 回调拿 code → 换 user_access_token 存 SQLite；周期性刷新临近过期的 token。
// 授权成功后飞书 connector（user 模式）才能以该用户身份拉其云文档。
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// Config 授权 webhook 配置。
type Config struct {
	Addr            string        // 监听地址，如 :8089
	ExternalURL     string        // 外部可访问地址（拼 redirect_uri；空则用请求推断）
	RefreshInterval time.Duration // token 刷新巡检间隔
}

// Service 授权服务。
type Service struct {
	cfg    Config
	fs     *feishu.Client
	store  *storage.Store
	server *http.Server
}

func New(cfg Config, fs *feishu.Client, store *storage.Store) *Service {
	return &Service{cfg: cfg, fs: fs, store: store}
}

// redirectURI 回调地址（需与飞书后台"安全设置-重定向 URL"配置完全一致）。
func (s *Service) redirectURI(r *http.Request) string {
	base := s.cfg.ExternalURL
	if base == "" && r != nil {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
		base = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	return base + "/oauth/feishu/callback"
}

// Handler HTTP 路由。
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/feishu/start", s.handleStart)
	mux.HandleFunc("/oauth/feishu/callback", s.handleCallback)
	mux.HandleFunc("/oauth/status", s.handleStatus)
	mux.HandleFunc("/oauth/feishu/refresh", s.handleRefreshNow) // 手动触发刷新（调试用）
	return mux
}

// handleStart 生成授权链接并跳转（或返回链接供复制）。
// 用法：/oauth/feishu/start           → 302 跳转到飞书授权页
//       /oauth/feishu/start?show=1    → 返回链接文本（便于复制发给员工）
func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	state := fmt.Sprintf("%d", time.Now().UnixNano())
	authURL := feishu.AuthorizeURL(s.fs.AppID, s.redirectURI(r), state)
	if r.URL.Query().Get("show") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "请用飞书打开以下链接完成授权：\n\n%s\n", authURL)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleCallback 飞书 OAuth 回调：拿 code 换 token 存 SQLite。
func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		s.html(w, 400, "回调缺少 code 参数")
		return
	}
	tok, err := s.fs.ExchangeCode(code, s.redirectURI(r))
	if err != nil {
		s.html(w, 500, "code 换 token 失败: "+err.Error())
		return
	}

	// 确认授权人身份
	openID, unionID, name, err := s.fs.UserInfo(tok.AccessToken)
	if err != nil {
		log.Printf("[auth] 拿用户信息失败（仍保存 token）: %v", err)
	}
	externalID := openID
	if externalID == "" {
		externalID = unionID
	}
	if externalID == "" {
		externalID = "unknown_" + fmt.Sprint(time.Now().Unix())
	}

	now := time.Now().Unix()
	// refresh_token 的有效期以接口返回为准；接口没给（未授予 offline_access，
	// 或字段缺失）时留 0 表示「未知」，而不是写成 now（那等于立刻过期）。
	var refreshExpireAt int64
	if tok.RefreshExpiresIn > 0 {
		refreshExpireAt = now + tok.RefreshExpiresIn
	}

	grant := &storage.OAuthGrant{
		Platform:        "feishu",
		ExternalUserID:  externalID,
		Code:            code,
		AccessToken:     tok.AccessToken,
		RefreshToken:    tok.RefreshToken,
		ExpireAt:        now + tok.ExpiresIn,
		RefreshExpireAt: refreshExpireAt,
		Status:          "authorized",
	}
	if err := s.store.UpsertOAuthGrant(grant); err != nil {
		s.html(w, 500, "保存授权失败: "+err.Error())
		return
	}
	log.Printf("[auth] 飞书授权成功: %s（%s）token 有效至 %s", name, externalID,
		time.Unix(grant.ExpireAt, 0).Format("01-02 15:04"))
	s.html(w, 200, fmt.Sprintf("授权成功，%s！可以关闭此页面了。", name))
}

// handleStatus 列出授权情况（谁已授权、token 何时过期）。
func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	grants, err := s.store.ListOAuthGrants("feishu")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type row struct {
		UserID    string `json:"user_id"`
		Status    string `json:"status"`
		ExpireAt  string `json:"token_expire_at"`
		RefreshAt string `json:"refresh_expire_at"`
		Valid     bool   `json:"token_valid_now"`
	}
	now := time.Now().Unix()
	rows := make([]row, 0, len(grants))
	for _, g := range grants {
		rows = append(rows, row{
			UserID:    g.ExternalUserID,
			Status:    g.Status,
			ExpireAt:  time.Unix(g.ExpireAt, 0).Format("2006-01-02 15:04:05"),
			RefreshAt: time.Unix(g.RefreshExpireAt, 0).Format("2006-01-02 15:04:05"),
			Valid:     g.ExpireAt > now && g.Status == "authorized",
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"count": len(rows), "grants": rows})
}

// handleRefreshNow 手动触发一轮刷新（调试）。
func (s *Service) handleRefreshNow(w http.ResponseWriter, r *http.Request) {
	n := s.refreshExpiring()
	fmt.Fprintf(w, "已刷新 %d 个 token\n", n)
}

// Start 启动 HTTP 服务（goroutine 内调用）。
func (s *Service) Start() error {
	s.server = &http.Server{Addr: s.cfg.Addr, Handler: s.Handler()}
	log.Printf("[auth] OAuth webhook 监听 %s（回调地址需与飞书后台一致）", s.cfg.Addr)
	return s.server.ListenAndServe()
}

// Stop 优雅关闭。
func (s *Service) Stop(ctx context.Context) {
	if s.server != nil {
		s.server.Shutdown(ctx)
	}
}

// RefreshLoop 周期性刷新临近过期的 token（实现 engine.Task 接口）。
func (s *Service) Name() string                 { return "feishu-token-refresher" }
func (s *Service) Interval() time.Duration      { return s.cfg.RefreshInterval }
func (s *Service) Run(ctx context.Context) error {
	s.refreshExpiring()
	return nil
}

// refreshExpiring 刷新所有临近过期（剩 <10 分钟）或已过期但 refresh_token 仍有效的授权。
func (s *Service) refreshExpiring() int {
	grants, err := s.store.ListOAuthGrants("feishu")
	if err != nil {
		log.Printf("[auth] 读取授权列表失败: %v", err)
		return 0
	}
	now := time.Now().Unix()
	refreshed := 0
	for i := range grants {
		g := &grants[i]
		if g.Status != "authorized" || g.RefreshToken == "" {
			continue
		}
		// RefreshExpireAt 为 0 表示历史记录没记下有效期（旧版本字段名写错导致解析为 0）。
		// 不能据此判定「已过期」——refresh_token 本身还在，直接尝试刷新一次即可，
		// 刷新成功会把正确的有效期补上。真正的过期由刷新接口返回错误来暴露。
		if g.RefreshExpireAt > 0 && g.RefreshExpireAt <= now {
			if g.Status != "expired" {
				g.Status = "expired"
				s.store.UpsertOAuthGrant(g)
				log.Printf("[auth] %s 的 refresh_token 已过期，需重新授权", g.ExternalUserID)
			}
			continue
		}
		// 还剩 10 分钟以上就不动
		if g.ExpireAt-now > 600 {
			continue
		}
		tok, err := s.fs.RefreshUserToken(g.RefreshToken)
		if err != nil {
			log.Printf("[auth] 刷新 %s token 失败: %v", g.ExternalUserID, err)
			continue
		}
		g.AccessToken = tok.AccessToken
		g.RefreshToken = tok.RefreshToken
		g.ExpireAt = now + tok.ExpiresIn
		if tok.RefreshExpiresIn > 0 {
			g.RefreshExpireAt = now + tok.RefreshExpiresIn
		}
		g.Code = "" // code 一次性，刷新后清掉
		if err := s.store.UpsertOAuthGrant(g); err != nil {
			log.Printf("[auth] 保存刷新后的 token 失败: %v", err)
			continue
		}
		refreshed++
		log.Printf("[auth] 已刷新 %s 的 token（新有效期至 %s）", g.ExternalUserID,
			time.Unix(g.ExpireAt, 0).Format("01-02 15:04"))
	}
	return refreshed
}

func (s *Service) html(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<html><body style='font-family:sans-serif;padding:2em'><h2>%s</h2></body></html>", text)
}
