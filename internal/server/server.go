package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/store"
)

// Options serve 模式（API 文件库 + 文件服务器）的运行参数。
type Options struct {
	Addr  string // 监听地址，如 :8080
	Token string // FastGPT 调用时携带的 Bearer token
	// 外部可访问地址（用于拼 previewUrl/read 链接）。
	// 例如经 cloudflared 暴露时填 https://xxx.trycloudflare.com；
	// 留空则从请求头推断（X-Forwarded-Proto/Host 或直连 Host）。
	ExternalURL string
}

// Backend 文件库后端：本地目录或群晖 NAS（WebAPI）。
type Backend interface {
	Get(id string) (*store.Node, error)
	Children(parentID string) ([]*store.Node, error)
	Search(key string) ([]*store.Node, error)
	// Open 打开文件字节流（previewUrl 下载的最终数据源）。
	Open(n *store.Node) (io.ReadCloser, error)
}

// LocalBackend 本地目录后端（包装 store.Store 补上 Open）。
type LocalBackend struct{ *store.Store }

func (l LocalBackend) Open(n *store.Node) (io.ReadCloser, error) { return l.Store.Open(n) }

// FastGPT 统一响应格式
type resp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type Server struct {
	opts    Options
	backend Backend
	mux     *http.ServeMux
}

func New(opts Options, b Backend) *Server {
	s := &Server{opts: opts, backend: b, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/file/list", s.withAuth(s.handleList))
	s.mux.HandleFunc("/v1/file/content", s.withAuth(s.handleContent))
	s.mux.HandleFunc("/v1/file/read", s.withAuth(s.handleRead))
	s.mux.HandleFunc("/v1/file/detail", s.withAuth(s.handleDetail))
	// download 允许 query token（FastGPT 拉 previewUrl 时不带 Authorization 头）
	s.mux.HandleFunc("/v1/file/download", s.withAuthOrQueryToken(s.handleDownload))
	s.mux.HandleFunc("/healthz", s.handleHealth)
}

func (s *Server) Handler() http.Handler { return s.mux }

// tokenValid 校验 Bearer header。
func (s *Server) tokenValid(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	return strings.HasPrefix(auth, prefix) && strings.TrimPrefix(auth, prefix) == s.opts.Token
}

// withAuth 仅接受 Authorization: Bearer <token>。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenValid(r) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// withAuthOrQueryToken Bearer 或 ?token= 均可（用于文件下载外链）。
func (s *Server) withAuthOrQueryToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenValid(r) && r.URL.Query().Get("token") != s.opts.Token {
			writeErr(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next(w, r)
	}
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

// fileURL 拼某个文件的下载外链（带 token，供 FastGPT/浏览器直接访问）。
func (s *Server) fileURL(r *http.Request, id string) string {
	q := url.Values{}
	q.Set("id", id)
	q.Set("token", s.opts.Token)
	return s.externalBase(r) + "/v1/file/download?" + q.Encode()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]string{"status": "ok"})
}

// POST /v1/file/list  body: {"parentId": "...", "searchKey": "..."}
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
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
	if strings.TrimSpace(body.SearchKey) != "" {
		nodes, err = s.backend.Search(body.SearchKey)
	} else {
		pid := ""
		if body.ParentID != nil {
			pid = *body.ParentID
		}
		nodes, err = s.backend.Children(pid)
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if nodes == nil {
		nodes = []*store.Node{}
	}
	writeOK(w, nodes)
}

// GET /v1/file/content?id=xx
// 改为返回 previewUrl：FastGPT 通过该外链下载原文件并自行解析（支持 PDF/Word/Excel 等），
// 本服务不再做文本提取，文件内容原样提供。
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := s.backend.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if node.Type != "file" {
		writeErr(w, http.StatusBadRequest, "node is not a file")
		return
	}
	writeOK(w, map[string]string{
		"title":      node.Name,
		"previewUrl": s.fileURL(r, id),
	})
}

// GET /v1/file/read?id=xx  返回原文访问链接
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, err := s.backend.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, map[string]string{
		"url": s.fileURL(r, id),
	})
}

// GET /v1/file/detail?id=xx
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := s.backend.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, node)
}

// GET /v1/file/download?id=xx&token=xx  原样返回文件字节流（供 previewUrl/read 使用）
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := s.backend.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if node.Type != "file" {
		writeErr(w, http.StatusBadRequest, "node is not a file")
		return
	}
	// inline 而非 attachment：浏览器可直接预览 PDF/图片/文本等，
	// 不支持预览的格式（如 docx）仍会自动下载；对 FastGPT 抓取无影响（它只读字节流）。
	// 中文文件名按 RFC 5987 编码，保证 FastGPT/浏览器正确解析文件名。
	encoded := url.PathEscape(node.Name)
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+encoded)
	rc, err := s.backend.Open(node)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "open file: "+err.Error())
		return
	}
	defer rc.Close()
	if _, err := io.Copy(w, rc); err != nil {
		// 响应头可能已写出，只能记日志
		_ = err
	}
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
