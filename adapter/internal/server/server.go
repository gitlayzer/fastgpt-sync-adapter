package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"fastgpt-sync-adapter/adapter/internal/config"
	"fastgpt-sync-adapter/adapter/internal/extract"
	"fastgpt-sync-adapter/adapter/internal/store"
)

// FastGPT 统一响应格式
type resp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type Server struct {
	cfg   *config.Config
	store *store.Store
	mux   *http.ServeMux
}

func New(cfg *config.Config, st *store.Store) *Server {
	s := &Server{cfg: cfg, store: st, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/file/list", s.withAuth(s.handleList))
	s.mux.HandleFunc("/v1/file/content", s.withAuth(s.handleContent))
	s.mux.HandleFunc("/v1/file/read", s.withAuth(s.handleRead))
	s.mux.HandleFunc("/v1/file/detail", s.withAuth(s.handleDetail))
	s.mux.HandleFunc("/v1/file/download", s.withAuth(s.handleDownload))
	s.mux.HandleFunc("/healthz", s.handleHealth)
}

func (s *Server) Handler() http.Handler { return s.mux }

// withAuth 校验 Authorization: Bearer <token>
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) || strings.TrimPrefix(auth, prefix) != s.cfg.Token {
			writeErr(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
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
		nodes, err = s.store.Search(body.SearchKey)
	} else {
		pid := ""
		if body.ParentID != nil {
			pid = *body.ParentID
		}
		nodes, err = s.store.Children(pid)
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
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if node.Type != "file" {
		writeErr(w, http.StatusBadRequest, "node is not a file")
		return
	}
	content, err := extract.Text(node.AbsPath())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read file: "+err.Error())
		return
	}
	writeOK(w, map[string]string{
		"title":   node.Name,
		"content": content,
	})
}

// GET /v1/file/read?id=xx  返回原文访问链接
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	if _, err := s.store.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// 提供一个可访问的下载链接。本地测试时返回本服务的下载端点。
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	url := scheme + "://" + host + "/v1/file/download?id=" + id
	writeOK(w, map[string]string{
		"url": url,
	})
}

// GET /v1/file/detail?id=xx
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, node)
}

// GET /v1/file/download?id=xx  原样返回文件字节流（供 read/previewUrl 使用）
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	node, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if node.Type != "file" {
		writeErr(w, http.StatusBadRequest, "node is not a file")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+node.Name+"\"")
	http.ServeFile(w, r, node.AbsPath())
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
