package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
	// OpenPath 按文件绝对路径直接打开字节流。
	OpenPath(filePath string) (io.ReadCloser, error)
}

// LocalBackend 本地目录后端（包装 store.Store 补上 Open）。
type LocalBackend struct{ *store.Store }

func (l LocalBackend) Open(n *store.Node) (io.ReadCloser, error) { return l.Store.Open(n) }
func (l LocalBackend) OpenPath(filePath string) (io.ReadCloser, error) {
	root := l.Store.Root()
	clean := filepath.Clean(filePath)
	if !strings.HasPrefix(clean, root) {
		clean = filepath.Clean(filepath.Join(root, clean))
	}
	if !strings.HasPrefix(clean, root) {
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
	s.mux.HandleFunc("/", s.handleIndex)
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
	pid := ""
	if strings.TrimSpace(body.SearchKey) != "" {
		nodes, err = s.backend.Search(body.SearchKey)
	} else {
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
	log.Printf("[server] POST /v1/file/list parentId=%q searchKey=%q -> 返回 %d 项 (来自 %s)", pid, body.SearchKey, len(nodes), r.RemoteAddr)
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
		log.Printf("[server] GET /v1/file/content id=%q (%s) -> 生成 previewUrl (来自 %s)", id, node.Name, r.RemoteAddr)
		writeOK(w, map[string]string{
			"title":      node.Name,
			"previewUrl": s.fileURL(r, id),
			"dirPath":    path.Dir(node.AbsPath()),
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

	// GET /v1/file/download?id=xx&token=xx 或 ?path=xx&token=xx  原样返回文件字节流（供 previewUrl/read/图片 使用）
	func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		rawPath := r.URL.Query().Get("path")
		var rc io.ReadCloser
		var fileName string
		var err error

		if id != "" {
			node, err := s.backend.Get(id)
			if err != nil {
				writeErr(w, http.StatusNotFound, err.Error())
				return
			}
			if node.Type != "file" {
				writeErr(w, http.StatusBadRequest, "node is not a file")
				return
			}
			fileName = node.Name
			log.Printf("[server] ⬇️ GET /v1/file/download 开始下载: %q (%s, 来自 %s)", node.Name, id, r.RemoteAddr)
			rc, err = s.backend.Open(node)
			if err != nil {
				writeErr(w, http.StatusBadGateway, "open file: "+err.Error())
				return
			}
		} else if rawPath != "" {
			fileName = path.Base(rawPath)
			log.Printf("[server] ⬇️ GET /v1/file/download 按路径下载: %q (来自 %s)", rawPath, r.RemoteAddr)
			rc, err = s.backend.OpenPath(rawPath)
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := strings.ReplaceAll(indexHTMLTemplate, "DEFAULT_TOKEN", s.opts.Token)
	w.Write([]byte(html))
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
        <input type="text" id="tokenInput" value="DEFAULT_TOKEN">
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
  token = e.target.value;
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
    // 渲染相对路径图片 ![图片](images/...)
    escaped = escaped.replace(/!\[图片\]\((images\/[^)]+)\)/g, function(_, imgRel) {
      const fullImgPath = (dirPath ? dirPath + '/' : '') + imgRel;
      const imgSrc = '/v1/file/download?path=' + encodeURIComponent(fullImgPath) + '&token=' + encodeURIComponent(token);
      return '<div style="margin:8px 0;"><a href="' + imgSrc + '" target="_blank" title="点击查看大图"><img src="' + imgSrc + '" alt="图片" style="max-width:380px;max-height:300px;border-radius:6px;border:1px solid #cbd5e1;box-shadow:0 1px 3px rgba(0,0,0,0.08);cursor:zoom-in;display:block;" loading="lazy" onerror="this.onerror=null;this.parentNode.innerHTML=\'<span style=\\\'color:#ef4444;font-size:12px;\\\'>[图片已过期或未找到]</span>\';" /></a></div>';
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

loadCurrent();
</script>
</body>
</html>
`
