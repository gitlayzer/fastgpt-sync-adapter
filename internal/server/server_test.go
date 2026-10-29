package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/store"
)

// ===== 测试基础设施 =====

// newTempRoot 建 <root>/张三/a.md、<root>/李四/b.md，返回 root 与两个员工目录。
func newTempRoot(t *testing.T) (root, zhangDir, liDir string) {
	t.Helper()
	root = t.TempDir()
	zhangDir = filepath.Join(root, "张三")
	liDir = filepath.Join(root, "李四")
	for _, d := range []string{zhangDir, liDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(zhangDir, "a.md"), []byte("# 张三的文档\nhello a\n"), 0o644); err != nil {
		t.Fatalf("write a.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liDir, "b.md"), []byte("# 李四的文档\nhello b\n"), 0o644); err != nil {
		t.Fatalf("write b.md: %v", err)
	}
	return root, zhangDir, liDir
}

// localFactory 用本地 store 构造按 root 隔离的 Backend（等价于生产里的 nasstore 工厂）。
func localFactory() func(root string) Backend {
	return func(root string) Backend {
		st, err := store.New(root)
		if err != nil {
			return nil
		}
		return LocalBackend{Store: st}
	}
}

// fakeAuth 极简 Authenticator：token → (personID, root)。
type fakeAuth struct {
	tokens map[string]struct{ person, root string }
}

func (f *fakeAuth) ResolveToken(token string) (string, string, bool, error) {
	e, ok := f.tokens[token]
	if !ok {
		return "", "", false, nil
	}
	return e.person, e.root, true, nil
}

func (f *fakeAuth) RootOf(personID string) (string, bool) {
	for _, e := range f.tokens {
		if e.person == personID {
			return e.root, true
		}
	}
	return "", false
}

const signTestSecret = "0123456789abcdef0123456789abcdef"

// testEnv 一套隔离测试环境。
type testEnv struct {
	srv      *Server
	ts       *httptest.Server
	root     string
	zhangDir string
	liDir    string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root, zhangDir, liDir := newTempRoot(t)
	auth := &fakeAuth{tokens: map[string]struct{ person, root string }{
		"t_zhang": {"wz", zhangDir},
		"t_li":    {"wl", liDir},
	}}
	s := New(Options{
		ExternalURL: "http://example.test", // 固定外部地址，便于断言 previewUrl 形状
		Auth:        auth,
		AdminToken:  "adm",
		SignSecret:  []byte(signTestSecret),
		NASRoot:     root,
	}, localFactory())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{srv: s, ts: ts, root: root, zhangDir: zhangDir, liDir: liDir}
}

// do 发起请求并返回响应与 body 文本。
func do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	_, _ = io.Copy(&buf, resp.Body)
	return resp, buf.String()
}

// get 发一个 GET（token 为空则不带 Authorization），返回状态码与 body。
func (e *testEnv) get(t *testing.T, pathAndQuery, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+pathAndQuery, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, body := do(t, req)
	return resp.StatusCode, body
}

// list 发 list 请求（POST /v1/file/list），返回状态码、body 与解析后的节点。
func (e *testEnv) list(t *testing.T, token string, parentID *string) (int, string, []store.Node) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"parentId": parentID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/file/list", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, body := do(t, req)
	var out struct {
		Data []store.Node `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, body, out.Data
}

// relID 计算节点 id：与 store.Store 同一套规则 id = sha1(相对 backend root 的路径)。
// 两个员工目录各自作为 root，故同一相对路径在两人下 id 相同但指向不同文件。
func relID(relPath string) string {
	return store.HashID(filepath.Join(relPath))
}

// ===== 1. 员工只能看到自己的目录 =====

func TestListScopedToPerson(t *testing.T) {
	e := newTestEnv(t)

	code, body, nodes := e.list(t, "t_zhang", nil)
	if code != http.StatusOK {
		t.Fatalf("张三 list 应 200，实际 %d: %s", code, body)
	}
	if len(nodes) != 1 {
		t.Fatalf("张三根目录应只有 1 项，实际 %d: %+v", len(nodes), nodes)
	}
	if nodes[0].Name != "a.md" {
		t.Fatalf("张三根目录应只看到 a.md，实际 %q", nodes[0].Name)
	}
	for _, n := range nodes {
		if strings.Contains(n.Name, "李四") || n.Name == "b.md" {
			t.Fatalf("张三看到了李四的内容: %+v", n)
		}
	}

	code, body, liNodes := e.list(t, "t_li", nil)
	if code != http.StatusOK {
		t.Fatalf("李四 list 应 200，实际 %d: %s", code, body)
	}
	if len(liNodes) != 1 || liNodes[0].Name != "b.md" {
		t.Fatalf("李四根目录应只有 b.md，实际 %+v", liNodes)
	}

	// 管理员（httptest 客户端来自 127.0.0.1）看得到两个员工目录，且看不到条目内的文件
	code, body, adminNodes := e.list(t, "adm", nil)
	if code != http.StatusOK {
		t.Fatalf("管理员 list 应 200，实际 %d: %s", code, body)
	}
	if len(adminNodes) != 2 {
		t.Fatalf("管理员根目录应有 2 个员工目录，实际 %d: %+v", len(adminNodes), adminNodes)
	}
}

// ===== 2. 跨员工 id 访问 → 404 =====

func TestCrossPersonIDNotFound(t *testing.T) {
	e := newTestEnv(t)

	liID := relID("b.md")

	for _, path := range []string{
		"/v1/file/detail?id=" + liID,
		"/v1/file/content?id=" + liID,
		"/v1/file/download?id=" + liID,
		"/v1/file/read?id=" + liID,
	} {
		code, body := e.get(t, path, "t_zhang")
		if code != http.StatusNotFound {
			t.Fatalf("张三访问李四的 %s 应 404，实际 %d: %s", path, code, body)
		}
	}

	// 对照：李四自己下载同一 id → 200 且内容正确
	code, body := e.get(t, "/v1/file/download?id="+liID, "t_li")
	if code != http.StatusOK {
		t.Fatalf("李四下载自己的文件应 200，实际 %d: %s", code, body)
	}
	if !strings.Contains(body, "hello b") {
		t.Fatalf("下载内容不对: %q", body)
	}

	// 反向对照：张三自己的 a.md 正常
	code, body = e.get(t, "/v1/file/detail?id="+relID("a.md"), "t_zhang")
	if code != http.StatusOK {
		t.Fatalf("张三取自己的 detail 应 200，实际 %d: %s", code, body)
	}
}

// ===== 3. 无效 token → 401 =====

func TestInvalidToken401(t *testing.T) {
	e := newTestEnv(t)

	if code, body, _ := e.list(t, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("无 header 应 401，实际 %d: %s", code, body)
	}
	if code, body, _ := e.list(t, "wrong-token", nil); code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401，实际 %d: %s", code, body)
	}
	if code, body := e.get(t, "/v1/file/detail?id=x", ""); code != http.StatusUnauthorized {
		t.Fatalf("detail 无 header 应 401，实际 %d: %s", code, body)
	}
	if code, body := e.get(t, "/v1/file/download?id=x", ""); code != http.StatusUnauthorized {
		t.Fatalf("download 无 header 应 401，实际 %d: %s", code, body)
	}
}

// ===== 4. 管理员 token + 来源网段 =====

func TestAdminToken(t *testing.T) {
	root, zhangDir, liDir := newTempRoot(t)
	auth := &fakeAuth{tokens: map[string]struct{ person, root string }{
		"t_zhang": {"wz", zhangDir},
		"t_li":    {"wl", liDir},
	}}
	s := New(Options{
		Auth:       auth,
		AdminToken: "adm",
		SignSecret: []byte(signTestSecret),
		NASRoot:    root,
	}, localFactory())

	// 通过真实网络请求：RemoteAddr = 127.0.0.1，属于回环 → 管理员可用
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/file/list", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer adm")
	resp, body := do(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("回环管理员 list 应 200，实际 %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Data []store.Node `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if len(out.Data) != 2 {
		t.Fatalf("管理员根目录应有 2 项，实际 %d: %+v", len(out.Data), out.Data)
	}

	// 直接打 handler 并伪造 RemoteAddr=8.8.8.8 → 403
	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/file/list", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer adm")
	req.RemoteAddr = "8.8.8.8:1234"
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非允许网段的管理员应 403，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// AdminToken 为空：同一个 "adm" 落到员工通道解析失败 → 401
	s2 := New(Options{
		Auth:       auth,
		SignSecret: []byte(signTestSecret),
		NASRoot:    root,
	}, localFactory())
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/file/list", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer adm")
	req.RemoteAddr = "127.0.0.1:1234"
	s2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("AdminToken 为空时应 401，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// AdminAllow 显式配置：只允许 8.8.8.8/32 时，回环管理员 → 403
	s3 := New(Options{
		Auth:       auth,
		AdminToken: "adm",
		AdminAllow: []string{"8.8.8.8/32"},
		SignSecret: []byte(signTestSecret),
		NASRoot:    root,
	}, localFactory())
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/file/list", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer adm")
	req.RemoteAddr = "127.0.0.1:1234"
	s3.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("AdminAllow 不含回环时应 403，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// ===== 5. 签名下载链接 =====

func TestSignedLink(t *testing.T) {
	e := newTestEnv(t)
	zhangID := relID("a.md")

	code, body := e.get(t, "/v1/file/content?id="+zhangID, "t_zhang")
	if code != http.StatusOK {
		t.Fatalf("content 应 200，实际 %d: %s", code, body)
	}
	var contentResp struct {
		Data struct {
			PreviewURL string `json:"previewUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &contentResp); err != nil {
		t.Fatalf("解析 content: %v (%s)", err, body)
	}
	previewURL := contentResp.Data.PreviewURL
	if previewURL == "" {
		t.Fatalf("previewUrl 为空: %s", body)
	}
	if strings.Contains(previewURL, "token=") {
		t.Fatalf("previewUrl 不应包含 token=: %s", previewURL)
	}
	// p=员工 ID 是签名载荷的一部分（不是机密），但明文 token 与管理员 token 绝不能出现。
	for _, secret := range []string{"t_zhang", "t_li", "adm"} {
		if strings.Contains(previewURL, secret) {
			t.Fatalf("previewUrl 泄露了 %q: %s", secret, previewURL)
		}
	}

	u, err := url.Parse(previewURL)
	if err != nil {
		t.Fatalf("parse previewUrl: %v", err)
	}
	q := u.Query()
	for _, k := range []string{"id", "p", "exp", "sig"} {
		if q.Get(k) == "" {
			t.Fatalf("previewUrl 缺少参数 %q: %s", k, previewURL)
		}
	}
	if q.Get("p") != "wz" {
		t.Fatalf("previewUrl 的 p 应为员工 ID wz，实际 %q", q.Get("p"))
	}

	// 直接 GET 签名链接（不带 Authorization）→ 200 且内容正确
	code, body = e.get(t, u.Path+"?"+u.RawQuery, "")
	if code != http.StatusOK {
		t.Fatalf("签名链接应 200，实际 %d: %s", code, body)
	}
	if !strings.Contains(body, "hello a") {
		t.Fatalf("签名链接下载内容不对: %q", body)
	}

	// 篡改 sig → 403
	tampered := cloneQuery(q)
	tampered.Set("sig", strings.Repeat("A", len(q.Get("sig"))))
	if code, body = e.get(t, u.Path+"?"+tampered.Encode(), ""); code != http.StatusForbidden {
		t.Fatalf("篡改 sig 应 403，实际 %d: %s", code, body)
	}

	// 篡改 id → 403
	tampered = cloneQuery(q)
	tampered.Set("id", store.HashID("/other.md"))
	if code, body = e.get(t, u.Path+"?"+tampered.Encode(), ""); code != http.StatusForbidden {
		t.Fatalf("篡改 id 应 403，实际 %d: %s", code, body)
	}

	// 篡改 p（换成另一个员工）→ 403
	tampered = cloneQuery(q)
	tampered.Set("p", "wl")
	if code, body = e.get(t, u.Path+"?"+tampered.Encode(), ""); code != http.StatusForbidden {
		t.Fatalf("篡改 p 应 403，实际 %d: %s", code, body)
	}

	// exp 过期 → 403
	tampered = cloneQuery(q)
	tampered.Set("exp", "1")
	if code, body = e.get(t, u.Path+"?"+tampered.Encode(), ""); code != http.StatusForbidden {
		t.Fatalf("过期链接应 403，实际 %d: %s", code, body)
	}

	// 旧 ?token= 查询参数鉴权已删除 → 无 sig 时按 Bearer 走，未带 Bearer 即 401
	if code, body = e.get(t, u.Path+"?id="+zhangID+"&token=t_zhang", ""); code != http.StatusUnauthorized {
		t.Fatalf("?token= 鉴权应已删除（期望 401），实际 %d: %s", code, body)
	}
}

func cloneQuery(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// ===== 6. 按路径下载仅管理员 =====

func TestPathDownloadAdminOnly(t *testing.T) {
	e := newTestEnv(t)
	target := filepath.Join(e.zhangDir, "a.md")
	qpath := "/v1/file/download?path=" + url.QueryEscape(target)

	for _, token := range []string{"t_zhang", "t_li"} {
		code, body := e.get(t, qpath, token)
		if code != http.StatusForbidden {
			t.Fatalf("员工 %s 使用 ?path= 应 403，实际 %d: %s", token, code, body)
		}
	}

	// 签名链接不支持 path（带 sig + path → 403）
	v := e.srv.signLink("ignored", "wz", time.Now())
	code, body := e.get(t, "/v1/file/download?path="+url.QueryEscape(target)+"&"+v.Encode(), "")
	if code != http.StatusForbidden {
		t.Fatalf("签名链接带 path 应 403，实际 %d: %s", code, body)
	}

	// 无凭据 → 401
	if code, body := e.get(t, qpath, ""); code != http.StatusUnauthorized {
		t.Fatalf("无凭据 ?path= 应 401，实际 %d: %s", code, body)
	}

	// 管理员（回环）可以按路径下载
	code, body = e.get(t, qpath, "adm")
	if code != http.StatusOK {
		t.Fatalf("管理员按路径下载应 200，实际 %d: %s", code, body)
	}
	if !strings.Contains(body, "hello a") {
		t.Fatalf("管理员按路径下载内容不对: %q", body)
	}
}

// ===== 7. 首页不含任何 token =====

func TestIndexHasNoToken(t *testing.T) {
	e := newTestEnv(t)

	code, body := e.get(t, "/", "")
	if code != http.StatusOK {
		t.Fatalf("GET / 应 200，实际 %d", code)
	}
	for _, secret := range []string{"adm", "t_zhang", "t_li", "DEFAULT_TOKEN"} {
		if strings.Contains(body, secret) {
			t.Fatalf("首页泄露了 %q", secret)
		}
	}
	if !strings.Contains(body, `id="tokenInput"`) {
		t.Fatalf("首页应保留手动输入 token 的输入框")
	}

	// /healthz 保持不鉴权
	if code, body := e.get(t, "/healthz", ""); code != http.StatusOK {
		t.Fatalf("/healthz 应 200，实际 %d: %s", code, body)
	}
}
