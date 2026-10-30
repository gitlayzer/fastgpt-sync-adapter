package wecomcontact

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// stubServer 可换响应的企微 mock：token 固定可用，externalcontact/get 由 handler 决定。
type stubServer struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    int
	response func(id string) (int, map[string]any) // (HTTP 状态码，0=200；响应体)
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gettoken" {
			writeJSON(t, w, map[string]any{"errcode": 0, "errmsg": "ok", "access_token": "tok", "expires_in": 7200})
			return
		}
		s.mu.Lock()
		s.calls++
		resp := s.response
		s.mu.Unlock()
		if resp == nil {
			t.Errorf("未设置响应")
			http.NotFound(w, r)
			return
		}
		status, body := resp(r.URL.Query().Get("external_userid"))
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Errorf("编码响应: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubServer) setResponse(f func(id string) (int, map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.response = f
}

func (s *stubServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// contactBody 构造 externalcontact/get 成功响应（corpName 非空=企微客户）。
func contactBody(id, name, corpName string) map[string]any {
	contact := map[string]any{"external_userid": id, "name": name, "type": 1}
	if corpName != "" {
		contact["type"] = 2
		contact["corp_name"] = corpName
	}
	return map[string]any{"errcode": 0, "errmsg": "ok", "external_contact": contact}
}

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newResolver 用给定库和时间源建解析器（时间源可注入，模拟缓存过期）。
func newResolver(srv *httptest.Server, st *storage.Store, now *time.Time) *Resolver {
	r := NewResolver(st, newTestClient(srv))
	if now != nil {
		r.now = func() time.Time { return *now }
	}
	return r
}

// 企业客户：Display/FileLabel 带 @公司；微信客户无 corp_name 则不带。
func TestResolverDisplayAndLabel(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		switch id {
		case "woCORP001":
			return 0, contactBody(id, "张三", "某公司")
		case "wmWX002":
			return 0, contactBody(id, "李四", "")
		}
		return 0, map[string]any{"errcode": 84061, "errmsg": "not external contact"}
	})
	r := newResolver(s.srv, newTestStore(t), nil)
	ctx := context.Background()

	if got := r.Display(ctx, "woCORP001"); got != "外部:张三@某公司" {
		t.Fatalf("企业客户 Display=%q", got)
	}
	if got := r.FileLabel(ctx, "woCORP001"); got != "张三@某公司_P001" {
		t.Fatalf("企业客户 FileLabel=%q，期望 张三@某公司_<后4位>", got)
	}
	if got := r.Display(ctx, "wmWX002"); got != "外部:李四" {
		t.Fatalf("微信客户 Display=%q", got)
	}
	if got := r.FileLabel(ctx, "wmWX002"); got != "李四_X002" {
		t.Fatalf("微信客户 FileLabel=%q，期望 李四_<后4位>", got)
	}
	if !IsExternal("wmX") || !IsExternal("woY") || IsExternal("zhangsan") || IsExternal("") {
		t.Fatal("IsExternal 判断不符")
	}
}

// 84061：显示 ID 后 6 位；24 小时内再查不调接口（换新 Resolver 走 SQLite 缓存也不行）。
func TestResolverNotFoundCached(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		return 0, map[string]any{"errcode": 84061, "errmsg": "not external contact"}
	})
	now := time.Now()
	st := newTestStore(t)
	r := newResolver(s.srv, st, &now)
	ctx := context.Background()
	const id = "wmGONE000123"

	if got := r.Display(ctx, id); got != "外部:000123" {
		t.Fatalf("Display=%q，期望 外部:<后6位>", got)
	}
	if got := r.FileLabel(ctx, id); got != "外部联系人_000123" {
		t.Fatalf("FileLabel=%q，期望 外部联系人_<后6位>", got)
	}
	if n := s.callCount(); n != 1 {
		t.Fatalf("首次查应调接口 1 次，实际 %d", n)
	}
	saved, ok, err := st.GetExternalContact(id)
	if err != nil || !ok {
		t.Fatalf("GetExternalContact: %v ok=%v", err, ok)
	}
	if saved.Status != "notfound" || saved.Label != "" {
		t.Fatalf("notfound 记录不符: %+v", saved)
	}

	// 12 小时后，新 Resolver（空内存）读 SQLite 命中，不再调接口
	now = now.Add(12 * time.Hour)
	r2 := newResolver(s.srv, st, &now)
	if got := r2.Display(ctx, id); got != "外部:000123" {
		t.Fatalf("缓存后 Display=%q", got)
	}
	if n := s.callCount(); n != 1 {
		t.Fatalf("24 小时内不应再调接口，实际调了 %d 次", n)
	}

	// 超过 24 小时（共 32h）再查：重新调接口
	now = now.Add(20 * time.Hour)
	newResolver(s.srv, st, &now).Display(ctx, id)
	if n := s.callCount(); n != 2 {
		t.Fatalf("超过 24 小时应重新调接口，实际 %d 次", n)
	}
}

// 改名：8 天后重新查到新名字 → Display 用新名字，FileLabel 保持旧标签。
func TestResolverRenameKeepsLabel(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		return 0, contactBody(id, "张三", "某公司")
	})
	st := newTestStore(t)
	ctx := context.Background()
	const id = "woRENAME01"

	// 首次查询发生在 8 天前（写入 fetched_at），之后缓存已过期
	old := time.Now().Add(-8 * 24 * time.Hour)
	label := newResolver(s.srv, st, &old).FileLabel(ctx, id)
	if label != "张三@某公司_ME01" {
		t.Fatalf("首次 label=%q", label)
	}
	s.setResponse(func(id string) (int, map[string]any) {
		return 0, contactBody(id, "张三丰", "某公司")
	})

	now := time.Now()
	r2 := newResolver(s.srv, st, &now)
	if got := r2.Display(ctx, id); got != "外部:张三丰@某公司" {
		t.Fatalf("改名后 Display=%q", got)
	}
	if got := r2.FileLabel(ctx, id); got != label {
		t.Fatalf("改名后 FileLabel 应保持旧标签 %q，实际 %q", label, got)
	}
}

// 接口临时错误：库里有旧 ok 记录 → 用旧记录，且不写库（fetched_at 不变）。
func TestResolverTempErrorFallsBackToCache(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		return 0, contactBody(id, "王五", "")
	})
	st := newTestStore(t)
	ctx := context.Background()
	const id = "wmTMP0001"

	// 首次查询发生在 8 天前，之后缓存已过期
	old := time.Now().Add(-8 * 24 * time.Hour)
	oldFetched := old.Unix()
	if got := newResolver(s.srv, st, &old).Display(ctx, id); got != "外部:王五" {
		t.Fatalf("首次 Display=%q", got)
	}
	s.setResponse(func(id string) (int, map[string]any) {
		return 500, nil
	})

	now := time.Now()
	r2 := newResolver(s.srv, st, &now)
	if got := r2.Display(ctx, id); got != "外部:王五" {
		t.Fatalf("临时错误应回退旧记录，Display=%q", got)
	}
	if got := r2.FileLabel(ctx, id); got != "王五_0001" {
		t.Fatalf("临时错误应回退旧 label，FileLabel=%q", got)
	}
	saved, ok, err := st.GetExternalContact(id)
	if err != nil || !ok {
		t.Fatalf("GetExternalContact: %v ok=%v", err, ok)
	}
	if saved.FetchedAt != oldFetched {
		t.Fatalf("临时错误不应写库，fetched_at=%d 期望 %d", saved.FetchedAt, oldFetched)
	}
}

// 没有缓存记录 + 临时错误：当查不到，且不写库。
func TestResolverTempErrorWithoutCache(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) { return 500, nil })
	st := newTestStore(t)
	r := newResolver(s.srv, st, nil)
	ctx := context.Background()
	const id = "wmNOCACHE01"

	if got := r.Display(ctx, id); got != "外部:ACHE01" {
		t.Fatalf("Display=%q，期望 外部:<后6位>", got)
	}
	if got := r.FileLabel(ctx, id); got != "外部联系人_ACHE01" {
		t.Fatalf("FileLabel=%q，期望 外部联系人_<后6位>", got)
	}
	if _, ok, err := st.GetExternalContact(id); err != nil || ok {
		t.Fatalf("临时错误不应写库: ok=%v err=%v", ok, err)
	}
}

// 非外部联系人 ID（内部员工）不查接口。
func TestResolverIgnoresInternalID(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		t.Errorf("内部员工 ID 不应调接口: %s", id)
		return 0, contactBody(id, "x", "")
	})
	r := newResolver(s.srv, newTestStore(t), nil)
	_ = r.Display(context.Background(), "zhangsan")
	if n := s.callCount(); n != 0 {
		t.Fatalf("不应调接口，实际 %d 次", n)
	}
}

// stripPhone：去掉手机号并清理首尾连接符。
func TestStripPhone(t *testing.T) {
	cases := []struct{ in, want string }{
		// 昵称结尾带手机号：只去号码
		{"张三13800000000", "张三"},
		// 只有手机号：结果为空
		{"13800000000", ""},
		// 只去号码本身、再 Trim 首尾，所以中间留下 "张三--销售"
		{"张三-13800000000-销售", "张三--销售"},
		// 号码前紧挨数字：不是独立手机号，不动
		{"订单号213800000000x", "订单号213800000000x"},
		// 号码中间有空格：不是 11 位连续数字，不动
		{"张三 138 0000 0000", "张三 138 0000 0000"},
		// 连续多个号码都去掉
		{"13800000000,13900000000", ""},
		{"张三 13800000000 13911111111", "张三"},
		// 首尾空白与连接符清理
		{"  13800000000- ", ""},
		{"张三", "张三"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripPhone(c.in); got != c.want {
			t.Errorf("stripPhone(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
}

// buildLabel：名字里的手机号不进文件名标签。
func TestBuildLabelStripsPhone(t *testing.T) {
	cases := []struct{ name, corpName, id, want string }{
		{"张三13800000000", "", "wmAAAA1234", "张三_1234"},
		{"李四13800000000", "某公司", "woBBBB5678", "李四@某公司_5678"},
		// 名字去掉后为空：退化为 外部联系人
		{"13800000000", "", "wmCCCC4321", "外部联系人_4321"},
		{" 13800000000 ", "某公司", "woDDDD8765", "外部联系人@某公司_8765"},
		// 企业名里带手机号也去掉
		{"张三", "某公司13800000000", "wmEEEE0001", "张三@某公司_0001"},
	}
	for _, c := range cases {
		if got := buildLabel(c.name, c.corpName, c.id); got != c.want {
			t.Errorf("buildLabel(%q,%q,%q)=%q，期望 %q", c.name, c.corpName, c.id, got, c.want)
		}
	}
}

// 接口返回昵称里带手机号：FileLabel 去号码，Display 保持原样。
func TestResolverLabelStripsPhoneButDisplayKeeps(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		switch id {
		case "wmPH01":
			return 0, contactBody(id, "张三13800000000", "")
		case "wmPH02":
			return 0, contactBody(id, "13800000000", "")
		}
		return 0, map[string]any{"errcode": 84061, "errmsg": "not external contact"}
	})
	r := newResolver(s.srv, newTestStore(t), nil)
	ctx := context.Background()

	if got := r.FileLabel(ctx, "wmPH01"); got != "张三_PH01" {
		t.Fatalf("FileLabel=%q，期望 张三_<后4位>", got)
	}
	if got := r.Display(ctx, "wmPH01"); got != "外部:张三13800000000" {
		t.Fatalf("Display=%q 应保持原样", got)
	}

	// 名字只有手机号：label 退化为 外部联系人_<后4位>
	if got := r.FileLabel(ctx, "wmPH02"); got != "外部联系人_PH02" {
		t.Fatalf("FileLabel=%q，期望 外部联系人_<后4位>", got)
	}
}

// 名字里带换行/非法字符时原样返回（清洗交给 NAS sink 的 sanitize）。
func TestResolverPassesNameVerbatim(t *testing.T) {
	s := newStubServer(t)
	s.setResponse(func(id string) (int, map[string]any) {
		return 0, contactBody(id, "张/三", "")
	})
	r := newResolver(s.srv, newTestStore(t), nil)
	ctx := context.Background()
	if got := r.Display(ctx, "wmSLASH01"); got != "外部:张/三" {
		t.Fatalf("Display=%q", got)
	}
	if got := r.FileLabel(ctx, "wmSLASH01"); !strings.Contains(got, "张/三") {
		t.Fatalf("FileLabel=%q 应原样保留非法字符", got)
	}
}
