package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/store"
)

// defaultLinkTTL 下载链接默认有效期。
const defaultLinkTTL = 10 * time.Minute

// Scope 一次请求能访问的范围。
type Scope struct {
	Admin    bool   // 管理员全量：根目录 = 整个 NAS root
	PersonID string // 员工 ID（Admin 时为空）
	Root     string // 该请求的根目录（NAS 绝对路径）
}

// name 日志用的调用方标识（Admin 写 admin）；永不包含 token。
func (sc *Scope) name() string {
	if sc == nil || sc.Admin {
		return adminPerson
	}
	return sc.PersonID
}

// Authenticator 由调用方实现：Bearer 明文 → 员工 → 员工根目录。
type Authenticator interface {
	// ResolveToken 返回员工 ID 与其 NAS 根目录；无效/吊销/过期返回 ok=false。
	ResolveToken(token string) (personID, root string, ok bool, err error)
	// RootOf 员工 ID → 当前根目录（签名下载链接校验时用，员工目录改名后仍能找到）。
	RootOf(personID string) (root string, ok bool)
}

// Options serve 模式（API 文件库 + 文件服务器）的运行参数。
type Options struct {
	Addr string // 监听地址，如 :8080
	// 外部可访问地址（用于拼 previewUrl/read 链接）。
	// 例如经 cloudflared 暴露时填 https://xxx.trycloudflare.com；
	// 留空则从请求头推断（X-Forwarded-Proto/Host 或直连 Host）。
	ExternalURL string
	// Auth 员工 token 解析器；为 nil 时只保留管理员通道。
	Auth Authenticator
	// AdminToken 管理员全量 token；为空表示关闭管理员通道。
	AdminToken string
	// AdminAllow 管理员请求允许的来源网段（CIDR），为空时只允许回环和私网地址
	//（10/8、172.16/12、192.168/16、127/8、::1、fc00::/7）。
	AdminAllow []string
	// SignSecret 下载链接签名密钥（>=32 字节）；为空时 New 内部用 crypto/rand 生成
	//（进程重启后旧链接失效，可接受）。
	SignSecret []byte
	// LinkTTL 下载链接有效期，0=10 分钟。
	LinkTTL time.Duration
	// NASRoot 整个 NAS 根路径（管理员 Scope.Root 用）。
	NASRoot string
}

// Backend 文件库后端：本地目录或群晖 NAS（WebAPI）。
type Backend interface {
	Get(id string) (*store.Node, error)
	Children(parentID string) ([]*store.Node, error)
	Search(key string) ([]*store.Node, error)
	// Open 打开文件字节流（previewUrl 下载的最终数据源）。
	Open(n *store.Node) (io.ReadCloser, error)
	// OpenPath 按文件绝对路径直接打开字节流。
	OpenPath(filePath string) (io.ReadCloser, error)
}

// LocalBackend 本地目录后端（包装 store.Store 补上 Open）。
type LocalBackend struct{ *store.Store }

func (l LocalBackend) Open(n *store.Node) (io.ReadCloser, error) { return l.Store.Open(n) }
func (l LocalBackend) OpenPath(filePath string) (io.ReadCloser, error) {
	root := filepath.Clean(l.Store.Root())
	clean := filepath.Clean(filePath)
	rel, err := filepath.Rel(root, clean)
	if err != nil || strings.HasPrefix(rel, "..") {
		clean = filepath.Clean(filepath.Join(root, clean))
		rel, err = filepath.Rel(root, clean)
	}
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("非法访问路径: %s", filePath)
	}
	return os.Open(clean)
}

// FastGPT 统一响应格式
type resp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type Server struct {
	opts       Options
	backendFor func(root string) Backend
	adminNets  []*net.IPNet
	mux        *http.ServeMux
}

// New 构造 Server。backendFor 按请求 scope 的根目录构造隔离的 Backend
// （nasstore 对应 func(root string) Backend { return nasstore.New(client, root) }）。
func New(opts Options, backendFor func(root string) Backend) *Server {
	if opts.LinkTTL <= 0 {
		opts.LinkTTL = defaultLinkTTL
	}
	if len(opts.SignSecret) == 0 {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Printf("[server] ⚠️ 生成下载链接签名密钥失败: %v", err)
		}
		opts.SignSecret = secret
	} else if len(opts.SignSecret) < 32 {
		log.Printf("[server] ⚠️ SignSecret 长度不足 32 字节（%d），建议使用更长的密钥", len(opts.SignSecret))
	}

	s := &Server{opts: opts, backendFor: backendFor, mux: http.NewServeMux()}
	for _, c := range opts.AdminAllow {
		_, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil {
			log.Printf("[server] ⚠️ 忽略非法 AdminAllow 网段 %q: %v", c, err)
			continue
		}
		s.adminNets = append(s.adminNets, n)
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/file/list", s.withScope(s.handleList))
	s.mux.HandleFunc("/v1/file/content", s.withScope(s.handleContent))
	s.mux.HandleFunc("/v1/file/read", s.withScope(s.handleRead))
	s.mux.HandleFunc("/v1/file/detail", s.withScope(s.handleDetail))
	// download 支持签名外链（?sig=，FastGPT 拉 previewUrl 时不带 Authorization 头）
	// 或 Bearer；旧的 ?token= 查询参数鉴权已删除。
	s.mux.HandleFunc("/v1/file/download", s.handleDownload)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/", s.handleIndex)
}

func (s *Server) Handler() http.Handler { return s.mux }

// scopeOf 解析请求的访问范围。返回非 0 状态码表示应直接以该状态码拒绝。
func (s *Server) scopeOf(r *http.Request) (*Scope, int) {
	const prefix = "Bearer "
	head := r.Header.Get("Authorization")
	if !strings.HasPrefix(head, prefix) {
		return nil, http.StatusUnauthorized
	}
	t := strings.TrimSpace(strings.TrimPrefix(head, prefix))
	if t == "" {
		return nil, http.StatusUnauthorized
	}

	// 管理员通道：常量时间比较，且只接受来自允许网段的请求（不信任 X-Forwarded-For）。
	if s.opts.AdminToken != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.opts.AdminToken)) == 1 {
		if !s.adminIPAllowed(r.RemoteAddr) {
			log.Printf("[server] ⛔ 管理员 token 来自非允许网段: %s", clientIP(r.RemoteAddr))
			return nil, http.StatusForbidden
		}
		return &Scope{Admin: true, Root: s.opts.NASRoot}, 0
	}

	if s.opts.Auth == nil {
		return nil, http.StatusUnauthorized
	}
	personID, root, ok, err := s.opts.Auth.ResolveToken(t)
	if err != nil {
		log.Printf("[server] ⚠️ token 解析失败: %v", err)
		return nil, http.StatusInternalServerError
	}
	if !ok || personID == "" || root == "" {
		return nil, http.StatusUnauthorized
	}
	return &Scope{PersonID: personID, Root: root}, 0
}

// withScope 鉴权后按 scope.Root 构造隔离的 Backend 传给 handler。
func (s *Server) withScope(next func(w http.ResponseWriter, r *http.Request, sc *Scope, b Backend)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, b, ok := s.resolve(w, r)
		if !ok {
			return
		}
		next(w, r, sc, b)
	}
}

// resolve 解析 scope 并构造 Backend。失败时已写好错误响应（ok=false）。
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (*Scope, Backend, bool) {
	sc, code := s.scopeOf(r)
	if code != 0 {
		writeErr(w, code, authErrMsg(code))
		return nil, nil, false
	}
	b := s.backend(sc.Root)
	if b == nil {
		writeErr(w, http.StatusInternalServerError, "backend unavailable")
		return nil, nil, false
	}
	return sc, b, true
}

// authErrMsg 鉴权失败对外的固定文案（不回显任何 token 信息）。
func authErrMsg(code int) string {
	switch code {
	case http.StatusForbidden:
		return "access not allowed from this address"
	case http.StatusInternalServerError:
		return "token resolve failed"
	default:
		return "invalid or missing bearer token"
	}
}

// backend 按根目录构造 Backend（工厂为 nil 或返回 nil 时视为不可用）。
func (s *Server) backend(root string) Backend {
	if s.backendFor == nil {
		return nil
	}
	return s.backendFor(root)
}

// adminIPAllowed 判断来源地址是否在管理员允许网段内。
// AdminAllow 为空时只放行回环与私网地址。
func (s *Server) adminIPAllowed(remoteAddr string) bool {
	ip := net.ParseIP(clientIP(remoteAddr))
	if ip == nil {
		return false
	}
	if len(s.adminNets) > 0 {
		for _, n := range s.adminNets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range defaultAdminNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// defaultAdminNets 未配置 AdminAllow 时放行的私网网段。
var defaultAdminNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "::1/128", "fc00::/7"} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// clientIP 从 RemoteAddr 取 IP（不信任任何代理头）。
func clientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// externalBase 计算对外可访问的 base URL。
func (s *Server) externalBase(r *http.Request) string {
	if s.opts.ExternalURL != "" {
		return strings.TrimRight(s.opts.ExternalURL, "/")
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// fileURL 拼某个文件的下载外链（带 HMAC 签名，不携带任何明文 token）。
func (s *Server) fileURL(r *http.Request, sc *Scope, id string) string {
	q := s.signLink(id, sc.name(), time.Now())
	return s.externalBase(r) + "/v1/file/download?" + q.Encode()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]string{"status": "ok"})
}

// POST /v1/file/list  body: {"parentId": "...", "searchKey": "..."}
// 只列当前 scope 的根目录（员工目录或 NAS 根）之下的内容。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request, sc *Scope, b Backend) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var body struct {
		ParentID  *string `json:"parentId"`
		SearchKey string  `json:"searchKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}

	var nodes []*store.Node
	var err error
	pid := ""
	if strings.TrimSpace(body.SearchKey) != "" {
		nodes, err = b.Search(body.SearchKey)
	} else {
		if body.ParentID != nil {
			pid = *body.ParentID
		}
		nodes, err = b.Children(pid)
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if nodes == nil {
		nodes = []*store.Node{}
	}
	log.Printf("[server] POST /v1/file/list person=%s parentId=%q searchKey=%q -> 返回 %d 项 (来自 %s)", sc.name(), pid, body.SearchKey, len(nodes), r.RemoteAddr)
	writeOK(w, nodes)
}

// GET /v1/file/content?id=xx
// 返回 previewUrl（HMAC 签名的下载外链）：FastGPT 通过该外链下载原文件并自行解析
// （支持 PDF/Word/Excel 等），本服务不再做文本提取，文件内容原样提供。
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request, sc *Scope, b Backend) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := b.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if node.Type != "file" {
		writeErr(w, http.StatusBadRequest, "node is not a file")
		return
	}
	log.Printf("[server] GET /v1/file/content person=%s id=%q (%s) -> 生成 previewUrl (来自 %s)", sc.name(), id, node.Name, r.RemoteAddr)
	writeOK(w, map[string]string{
		"title":      node.Name,
		"previewUrl": s.fileURL(r, sc, id),
		"dirPath":    path.Dir(node.AbsPath()),
	})
}

// GET /v1/file/read?id=xx  返回原文访问链接
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, sc *Scope, b Backend) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, err := b.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, map[string]string{
		"url": s.fileURL(r, sc, id),
	})
}

// GET /v1/file/detail?id=xx
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request, sc *Scope, b Backend) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := b.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, node)
}

// GET /v1/file/download?id=xx&sig=...  签名外链下载（无 sig 时走 Bearer）
// GET /v1/file/download?path=xx       仅管理员可用（按绝对路径下载，仅供预览页图片用）
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	rawPath := q.Get("path")

	var sc *Scope
	var b Backend

	switch {
	case q.Get("sig") != "":
		// 签名外链：id + exp + sig，不带任何 token。
		if id == "" || rawPath != "" {
			writeErr(w, http.StatusForbidden, errBadLink.Error())
			return
		}
		person, err := s.verifyLink(q, time.Now())
		if err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		if person == adminPerson {
			if s.opts.NASRoot == "" {
				writeErr(w, http.StatusNotFound, "node not found")
				return
			}
			sc = &Scope{Admin: true, Root: s.opts.NASRoot}
		} else {
			if s.opts.Auth == nil {
				writeErr(w, http.StatusNotFound, "node not found")
				return
			}
			root, ok := s.opts.Auth.RootOf(person)
			if !ok || root == "" {
				writeErr(w, http.StatusNotFound, "node not found")
				return
			}
			sc = &Scope{PersonID: person, Root: root}
		}
		b = s.backend(sc.Root)
	default:
		// Bearer 通道：与 list/content/detail 完全一致的 scope 隔离。
		resolved, resolvedBackend, ok := s.resolve(w, r)
		if !ok {
			return
		}
		sc, b = resolved, resolvedBackend
	}
	if b == nil {
		writeErr(w, http.StatusInternalServerError, "backend unavailable")
		return
	}

	var rc io.ReadCloser
	var fileName string
	var err error

	if id != "" {
		node, err := b.Get(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if node.Type != "file" {
			writeErr(w, http.StatusBadRequest, "node is not a file")
			return
		}
		fileName = node.Name
		log.Printf("[server] ⬇️ GET /v1/file/download person=%s 开始下载: %q (%s, 来自 %s)", sc.name(), node.Name, id, r.RemoteAddr)
		rc, err = b.Open(node)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "open file: "+err.Error())
			return
		}
	} else if rawPath != "" {
		// 按路径下载会绕过 id 隔离，只允许管理员 scope；签名链接不支持 path。
		if !sc.Admin {
			writeErr(w, http.StatusForbidden, "path download requires admin scope")
			return
		}
		fileName = path.Base(rawPath)
		log.Printf("[server] ⬇️ GET /v1/file/download person=%s 按路径下载: %q (来自 %s)", sc.name(), rawPath, r.RemoteAddr)
		rc, err = b.OpenPath(rawPath)
		if err != nil {
			writeErr(w, http.StatusNotFound, "open path: "+err.Error())
			return
		}
	} else {
		writeErr(w, http.StatusBadRequest, "missing id or path")
		return
	}
	defer rc.Close()

	ext := strings.ToLower(path.Ext(fileName))
	switch ext {
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".gif":
		w.Header().Set("Content-Type", "image/gif")
	case ".webp":
		w.Header().Set("Content-Type", "image/webp")
	case ".pdf":
		w.Header().Set("Content-Type", "application/pdf")
	case ".md", ".txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}

	encoded := url.PathEscape(fileName)
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+encoded)
	n, err := io.Copy(w, rc)
	if err != nil {
		log.Printf("[server] ⚠️ GET /v1/file/download 传输中断: %q: %v", fileName, err)
		return
	}
	log.Printf("[server] ✅ GET /v1/file/download 下载完成: %q (已传输 %.1f KB)", fileName, float64(n)/1024)
}

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp{Success: true, Message: "", Data: data})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp{Success: false, Message: msg, Data: nil})
}

// handleIndex 目录浏览页：纯静态壳，不含任何 token（使用者自行在输入框里填）。
// 页面本身不鉴权，所有数据接口仍需 Bearer。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(indexHTMLTemplate))
}

const indexHTMLTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>FastGPT Sync Adapter - NAS 文件库浏览器</title>
<style>
  :root {
    --primary: #2563eb;
    --bg: #f8fafc;
    --card: #ffffff;
    --border: #e2e8f0;
    --text: #0f172a;
    --text-muted: #64748b;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: var(--bg); color: var(--text); padding: 24px; line-height: 1.5; }
  .container { max-width: 1200px; margin: 0 auto; }
  .header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 24px; padding-bottom: 16px; border-bottom: 1px solid var(--border); flex-wrap: wrap; gap: 12px; }
  .title-group h1 { font-size: 22px; font-weight: 700; color: #1e293b; }
  .title-group p { font-size: 13px; color: var(--text-muted); margin-top: 4px; }
  .links { display: flex; gap: 8px; }
  .badge-link { font-size: 12px; padding: 6px 12px; background: #e0f2fe; color: #0369a1; border-radius: 6px; text-decoration: none; font-weight: 500; }
  .badge-link:hover { background: #bae6fd; }
  .card { background: var(--card); border: 1px solid var(--border); border-radius: 10px; box-shadow: 0 1px 3px rgba(0,0,0,0.05); overflow: hidden; }
  .toolbar { display: flex; align-items: center; justify-content: space-between; padding: 12px 16px; background: #f1f5f9; border-bottom: 1px solid var(--border); font-size: 13px; }
  .breadcrumb { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; font-size: 14px; }
  .breadcrumb-item { cursor: pointer; color: var(--primary); text-decoration: none; }
  .breadcrumb-item:hover { text-decoration: underline; }
  .breadcrumb-item.active { color: var(--text); font-weight: 600; cursor: default; }
  .breadcrumb-sep { color: var(--text-muted); }
  .token-bar { display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--text-muted); }
  .token-bar input { padding: 4px 8px; border: 1px solid var(--border); border-radius: 4px; font-size: 12px; width: 140px; }
  .file-list { list-style: none; }
  .file-item { display: flex; align-items: center; justify-content: space-between; padding: 12px 16px; border-bottom: 1px solid var(--border); cursor: pointer; transition: background 0.15s; }
  .file-item:last-child { border-bottom: none; }
  .file-item:hover { background: #f8fafc; }
  .file-info { display: flex; align-items: center; gap: 12px; flex: 1; min-width: 0; }
  .file-icon { font-size: 18px; width: 24px; text-align: center; }
  .file-name { font-size: 14px; font-weight: 500; color: #1e293b; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .file-meta { font-size: 12px; color: var(--text-muted); margin-left: 16px; white-space: nowrap; }
  .preview-panel { margin-top: 24px; background: var(--card); border: 1px solid var(--border); border-radius: 10px; padding: 20px; display: none; }
  .preview-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; padding-bottom: 12px; border-bottom: 1px solid var(--border); }
  .preview-title { font-size: 16px; font-weight: 600; }
  .preview-content { white-space: pre-wrap; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 13px; line-height: 1.6; background: #f8fafc; padding: 16px; border-radius: 6px; max-height: 500px; overflow-y: auto; border: 1px solid var(--border); }
  .loading { text-align: center; padding: 40px; color: var(--text-muted); font-size: 14px; }
  .empty { text-align: center; padding: 40px; color: var(--text-muted); font-size: 14px; }
</style>
</head>
<body>
<div class="container">
  <div class="header">
    <div class="title-group">
      <h1>FastGPT Sync Adapter</h1>
      <p>群晖 NAS 归档与 FastGPT API 文件库在线浏览</p>
    </div>
    <div class="links">
      <a class="badge-link" href="/healthz" target="_blank">健康状态</a>
      <a class="badge-link" href="http://127.0.0.1:8089/oauth/status" target="_blank">飞书 Token 状态</a>
      <a class="badge-link" href="http://127.0.0.1:8089/oauth/feishu/start" target="_blank">发起飞书授权</a>
    </div>
  </div>

  <div class="card">
    <div class="toolbar">
      <div class="breadcrumb" id="breadcrumb"></div>
      <div class="token-bar">
        <span>Token:</span>
        <input type="text" id="tokenInput" value="" placeholder="粘贴你的 API token" autocomplete="off">
        <button style="padding: 4px 8px; border: 1px solid var(--border); background: white; border-radius: 4px; cursor: pointer;" onclick="loadCurrent()">刷新</button>
      </div>
    </div>
    <div id="listContainer"></div>
  </div>

  <div class="preview-panel" id="previewPanel">
    <div class="preview-header">
      <div class="preview-title" id="previewTitle">文件预览</div>
      <a id="downloadBtn" href="#" target="_blank" style="font-size: 13px; color: var(--primary); text-decoration: none; font-weight: 500;">下载原文件 ↗</a>
    </div>
    <div class="preview-content" id="previewContent"></div>
  </div>
</div>

<script>
let token = document.getElementById('tokenInput').value;
let historyStack = [{ id: '', name: '根目录' }];

document.getElementById('tokenInput').addEventListener('change', (e) => {
  token = e.target.value.trim();
  historyStack = [{ id: '', name: '根目录' }];
  loadCurrent();
});

function renderBreadcrumb() {
  const bc = document.getElementById('breadcrumb');
  bc.innerHTML = '';
  historyStack.forEach((item, index) => {
    if (index > 0) {
      const sep = document.createElement('span');
      sep.className = 'breadcrumb-sep';
      sep.innerText = '/';
      bc.appendChild(sep);
    }
    const span = document.createElement('span');
    span.className = index === historyStack.length - 1 ? 'breadcrumb-item active' : 'breadcrumb-item';
    span.innerText = item.name;
    if (index < historyStack.length - 1) {
      span.onclick = () => {
        historyStack = historyStack.slice(0, index + 1);
        loadCurrent();
      };
    }
    bc.appendChild(span);
  });
}

async function loadCurrent() {
  renderBreadcrumb();
  const current = historyStack[historyStack.length - 1];
  const container = document.getElementById('listContainer');
  if (!token) {
    container.innerHTML = '<div class="empty">请先在上方输入你的 API token，然后点「刷新」。</div>';
    return;
  }
  container.innerHTML = '<div class="loading">正在加载 NAS 目录...</div>';

  try {
    const res = await fetch('/v1/file/list', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': 'Bearer ' + token
      },
      body: JSON.stringify({ parentId: current.id })
    });
    const data = await res.json();
    if (!data.success) {
      container.innerHTML = '<div class="empty" style="color: #ef4444;">加载失败: ' + (data.message || '未知错误') + '</div>';
      return;
    }
    const items = data.data || [];
    if (items.length === 0) {
      container.innerHTML = '<div class="empty">此目录为空</div>';
      return;
    }

    items.sort((a, b) => {
      if (a.type === b.type) return a.name.localeCompare(b.name);
      return a.type === 'folder' ? -1 : 1;
    });

    let html = '<ul class="file-list">';
    items.forEach(item => {
      const isFolder = item.type === 'folder';
      const icon = isFolder ? '📁' : (item.name.endsWith('.md') ? '📝' : (item.name.endsWith('.pdf') ? '📕' : '📄'));
      html += '<li class="file-item" onclick="handleItemClick(\'' + item.id + '\', \'' + escapeHtml(item.name) + '\', \'' + item.type + '\')">' +
        '<div class="file-info">' +
          '<span class="file-icon">' + icon + '</span>' +
          '<span class="file-name">' + escapeHtml(item.name) + '</span>' +
        '</div>' +
        '<div class="file-meta">' +
          (item.updateTime ? new Date(item.updateTime).toLocaleString('zh-CN') : '') +
        '</div>' +
      '</li>';
    });
    html += '</ul>';
    container.innerHTML = html;
  } catch (err) {
    container.innerHTML = '<div class="empty" style="color: #ef4444;">请求出错: ' + err.message + '</div>';
  }
}

async function handleItemClick(id, name, type) {
  if (type === 'folder') {
    historyStack.push({ id, name });
    loadCurrent();
    return;
  }

  const panel = document.getElementById('previewPanel');
  const title = document.getElementById('previewTitle');
  const content = document.getElementById('previewContent');
  const dl = document.getElementById('downloadBtn');

  panel.style.display = 'block';
  title.innerText = '正在载入: ' + name;
  content.innerText = '正在读取文件字节流...';
  dl.href = '#';

  try {
    const res = await fetch('/v1/file/content?id=' + encodeURIComponent(id), {
      headers: { 'Authorization': 'Bearer ' + token }
    });
    const data = await res.json();
    if (!data.success) {
      content.innerText = '获取预览失败: ' + data.message;
      return;
    }
    const previewUrl = data.data.previewUrl;
    title.innerText = name;
    dl.href = previewUrl;

    const lower = name.toLowerCase();
    if (lower.endsWith('.md')) {
      const fileRes = await fetch(previewUrl);
      const text = await fileRes.text();
      content.innerHTML = renderMarkdown(text, data.data.dirPath);
      loadInlineImages(content);
    } else if (lower.endsWith('.txt') || lower.endsWith('.json')) {
      const fileRes = await fetch(previewUrl);
      const text = await fileRes.text();
      content.innerText = text;
    } else if (lower.endsWith('.jpg') || lower.endsWith('.jpeg') || lower.endsWith('.png') || lower.endsWith('.gif') || lower.endsWith('.webp')) {
      content.innerHTML = '<div style="text-align:center;"><img src="' + escapeHtml(previewUrl) + '" style="max-width:100%;max-height:480px;object-fit:contain;border-radius:6px;box-shadow:0 1px 4px rgba(0,0,0,0.1);" /></div>';
    } else {
      content.innerText = '该文件为二进制文档 (' + name.split('.').pop() + ')，无法在文本框直接渲染。\n请点击右上角「下载原文件」按钮直接打开查阅。';
    }
  } catch (err) {
    content.innerText = '读取失败: ' + err.message;
  }
}

function renderMarkdown(md, dirPath) {
  const lines = md.split('\n');
  let html = '<div style="line-height:1.7;color:#1e293b;font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Helvetica,Arial,sans-serif;">';
  for (let rawLine of lines) {
    let line = rawLine.trim();
    if (!line) {
      html += '<div style="height:6px;"></div>';
      continue;
    }
    if (line.startsWith('# ')) {
      html += '<h2 style="font-size:18px;font-weight:700;margin:16px 0 8px;padding-bottom:6px;border-bottom:1px solid #e2e8f0;color:#0f172a;">' + escapeHtml(line.slice(2)) + '</h2>';
      continue;
    }
    if (line.startsWith('## ')) {
      html += '<div style="display:inline-block;background:#e0f2fe;color:#0369a1;font-size:12px;font-weight:600;padding:2px 8px;border-radius:4px;margin:12px 0 6px;">' + escapeHtml(line.slice(3)) + '</div>';
      continue;
    }
    let escaped = escapeHtml(line);
    // 渲染相对路径图片 ![图片](images/...)：图片字节流需要带 Bearer 拉取，再转 blob 显示（链接里不放 token）。
    escaped = escaped.replace(/!\[图片\]\((images\/[^)]+)\)/g, function(_, imgRel) {
      const fullImgPath = (dirPath ? dirPath + '/' : '') + imgRel;
      return '<div style="margin:8px 0;"><img data-path="' + encodeURIComponent(fullImgPath) + '" alt="图片" style="max-width:380px;max-height:300px;border-radius:6px;border:1px solid #cbd5e1;box-shadow:0 1px 3px rgba(0,0,0,0.08);display:block;" loading="lazy" /></div>';
    });
    // 渲染超链接 [链接：...](...)（严格限制为 http/https 协议，防止 javascript: XSS）
    escaped = escaped.replace(/\[链接：([^\]]+)\]\(([^)]*)\)/g, function(_, title, url) {
      const trimmed = url.trim();
      if (!/^https?:\/\//i.test(trimmed)) {
        return '<span style="color:#2563eb;font-weight:500;">[链接：' + title + ']</span>';
      }
      return '<a href="' + encodeURI(trimmed) + '" target="_blank" rel="noopener noreferrer" style="color:#2563eb;text-decoration:underline;font-weight:500;">[链接：' + title + '] ↗</a>';
    });
    // 渲染加粗 **...**
    escaped = escaped.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    // 渲染斜体 *...*
    escaped = escaped.replace(/\*([^*]+)\*/g, '<em>$1</em>');
    html += '<div style="margin:4px 0;font-size:13px;">' + escaped + '</div>';
  }
  html += '</div>';
  return html;
}

function escapeHtml(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// 预览里的内嵌图片：用 Bearer 取字节流后转 blob 渲染，避免把 token 写进任何 URL。
function loadInlineImages(rootEl) {
  rootEl.querySelectorAll('img[data-path]').forEach(async (img) => {
    const relPath = img.getAttribute('data-path');
    try {
      const res = await fetch('/v1/file/download?path=' + relPath, {
        headers: { 'Authorization': 'Bearer ' + token }
      });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const blob = await res.blob();
      img.src = URL.createObjectURL(blob);
      img.style.cursor = 'zoom-in';
      img.onclick = () => window.open(img.src, '_blank');
    } catch (e) {
      const tip = document.createElement('span');
      tip.style.color = '#ef4444';
      tip.style.fontSize = '12px';
      tip.innerText = '[图片已过期或未找到]';
      img.replaceWith(tip);
    }
  });
}

loadCurrent();
</script>
</body>
</html>
`
