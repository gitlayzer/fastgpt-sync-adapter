package wecomcontact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
)

// groupStub 客户群接口 mock：gettoken 固定可用，groupchat/get 由 handler 决定。
type groupStub struct {
	mu    sync.Mutex
	calls int
	// handler 返回 (响应体, HTTP 状态码；0=200)
	handler func(chatID string) (map[string]any, int)
}

func newGroupStub(t *testing.T) (*groupStub, *httptest.Server) {
	t.Helper()
	g := &groupStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gettoken":
			writeJSON(t, w, map[string]any{"errcode": 0, "errmsg": "ok", "access_token": "tok", "expires_in": 7200})
		case "/externalcontact/groupchat/get":
			if r.Method != http.MethodPost {
				t.Errorf("groupchat/get 应为 POST，实际 %s", r.Method)
			}
			var body struct {
				ChatID   string `json:"chat_id"`
				NeedName int    `json:"need_name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("解析请求体: %v", err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if body.NeedName != 1 {
				t.Errorf("need_name=%d，期望 1", body.NeedName)
			}
			g.mu.Lock()
			g.calls++
			h := g.handler
			g.mu.Unlock()
			if h == nil {
				t.Errorf("未设置 handler")
				http.NotFound(w, r)
				return
			}
			resp, status := h(body.ChatID)
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			writeJSON(t, w, resp)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *groupStub) setHandler(h func(chatID string) (map[string]any, int)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.handler = h
}

func (g *groupStub) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// groupBody 构造 groupchat/get 成功响应。
func groupBody(chatID, name string) map[string]any {
	return map[string]any{
		"errcode": 0, "errmsg": "ok",
		"group_chat": map[string]any{
			"chat_id":     chatID,
			"name":        name,
			"owner":       "zhangsan",
			"member_list": []any{},
		},
	}
}

func newGroupResolver(srv *httptest.Server, st *storage.Store, now *time.Time) *GroupResolver {
	r := NewGroupResolver(st, newTestClient(srv))
	if now != nil {
		r.now = func() time.Time { return *now }
	}
	return r
}

// 成功查到群名；7 天内再查走缓存，不再调接口。
func TestGroupResolverName(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		return groupBody(chatID, "江湾测试群"), 0
	})
	st := newTestStore(t)
	now := time.Now()
	r := newGroupResolver(srv, st, &now)
	ctx := context.Background()
	const room = "wrTESTROOM01"

	if got := r.GroupName(ctx, room); got != "江湾测试群" {
		t.Fatalf("GroupName=%q", got)
	}
	if n := g.callCount(); n != 1 {
		t.Fatalf("首次查应调接口 1 次，实际 %d", n)
	}
	saved, ok, err := st.GetGroupChat(room)
	if err != nil || !ok {
		t.Fatalf("GetGroupChat: %v ok=%v", err, ok)
	}
	if saved.Status != "ok" || saved.Name != "江湾测试群" || saved.FetchedAt != now.Unix() {
		t.Fatalf("缓存记录不符: %+v", saved)
	}

	// 3 天后新的 Resolver（空内存）读 SQLite 命中，不再调接口
	now = now.Add(3 * 24 * time.Hour)
	if got := newGroupResolver(srv, st, &now).GroupName(ctx, room); got != "江湾测试群" {
		t.Fatalf("缓存后 GroupName=%q", got)
	}
	if n := g.callCount(); n != 1 {
		t.Fatalf("7 天内不应再调接口，实际 %d 次", n)
	}

	// 超过 7 天（共 8 天）重新调接口
	now = now.Add(5 * 24 * time.Hour)
	newGroupResolver(srv, st, &now).GroupName(ctx, room)
	if n := g.callCount(); n != 2 {
		t.Fatalf("超过 7 天应重新调接口，实际 %d 次", n)
	}
}

// 群没起名（name 为空）：返回 ""，但记录仍是 ok（7 天内不重复查）。
func TestGroupResolverEmptyName(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		return groupBody(chatID, ""), 0
	})
	st := newTestStore(t)
	now := time.Now()
	r := newGroupResolver(srv, st, &now)
	ctx := context.Background()
	const room = "wrTESTNONAME"

	if got := r.GroupName(ctx, room); got != "" {
		t.Fatalf("无名群应返回空串，得到 %q", got)
	}
	saved, ok, err := st.GetGroupChat(room)
	if err != nil || !ok || saved.Status != "ok" || saved.Name != "" {
		t.Fatalf("无名群缓存记录不符: %+v ok=%v err=%v", saved, ok, err)
	}
	if got := newGroupResolver(srv, st, &now).GroupName(ctx, room); got != "" {
		t.Fatalf("缓存命中应仍为空串，得到 %q", got)
	}
	if n := g.callCount(); n != 1 {
		t.Fatalf("无名群 7 天内不应重复查，实际 %d 次", n)
	}
}

// 92002（无权限）：返回 ""，缓存 notfound，24 小时内不再调接口。
func TestGroupResolverNotFoundCached(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		return map[string]any{"errcode": 92002, "errmsg": "no permission"}, 0
	})
	st := newTestStore(t)
	now := time.Now()
	ctx := context.Background()
	const room = "wrTESTNOPERM"

	if got := newGroupResolver(srv, st, &now).GroupName(ctx, room); got != "" {
		t.Fatalf("无权限应返回空串，得到 %q", got)
	}
	saved, ok, err := st.GetGroupChat(room)
	if err != nil || !ok || saved.Status != "notfound" {
		t.Fatalf("应缓存 notfound: %+v ok=%v err=%v", saved, ok, err)
	}

	now = now.Add(12 * time.Hour)
	if got := newGroupResolver(srv, st, &now).GroupName(ctx, room); got != "" {
		t.Fatalf("缓存后应仍为空串，得到 %q", got)
	}
	if n := g.callCount(); n != 1 {
		t.Fatalf("24 小时内不应再调接口，实际 %d 次", n)
	}

	// 超过 24 小时（共 32h）重新查
	now = now.Add(20 * time.Hour)
	newGroupResolver(srv, st, &now).GroupName(ctx, room)
	if n := g.callCount(); n != 2 {
		t.Fatalf("超过 24 小时应重新调接口，实际 %d 次", n)
	}
}

// 90501（非客户群）也按 notfound 处理。
func TestGroupResolverNotGroupChat(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		return map[string]any{"errcode": 90501, "errmsg": "not a group chat"}, 0
	})
	st := newTestStore(t)
	ctx := context.Background()
	const room = "wrTESTINNER"

	if got := newGroupResolver(srv, st, nil).GroupName(ctx, room); got != "" {
		t.Fatalf("非客户群应返回空串，得到 %q", got)
	}
	if saved, ok, _ := st.GetGroupChat(room); !ok || saved.Status != "notfound" {
		t.Fatalf("应缓存 notfound: %+v ok=%v", saved, ok)
	}
}

// 临时错误：库里有旧 ok 记录 → 用旧名字，且不写库（fetched_at 不变）。
func TestGroupResolverTempErrorFallsBackToCache(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		return groupBody(chatID, "江湾测试群"), 0
	})
	st := newTestStore(t)
	ctx := context.Background()
	const room = "wrTESTTMP01"

	// 首次查询发生在 8 天前（缓存已过期）
	old := time.Now().Add(-8 * 24 * time.Hour)
	oldFetched := old.Unix()
	if got := newGroupResolver(srv, st, &old).GroupName(ctx, room); got != "江湾测试群" {
		t.Fatalf("首次 GroupName=%q", got)
	}
	g.setHandler(func(chatID string) (map[string]any, int) { return nil, 500 })

	now := time.Now()
	if got := newGroupResolver(srv, st, &now).GroupName(ctx, room); got != "江湾测试群" {
		t.Fatalf("临时错误应回退旧记录，GroupName=%q", got)
	}
	saved, ok, err := st.GetGroupChat(room)
	if err != nil || !ok {
		t.Fatalf("GetGroupChat: %v ok=%v", err, ok)
	}
	if saved.FetchedAt != oldFetched {
		t.Fatalf("临时错误不应写库，fetched_at=%d 期望 %d", saved.FetchedAt, oldFetched)
	}
}

// 没有缓存 + 临时错误：返回 ""，且不写库。
func TestGroupResolverTempErrorWithoutCache(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) { return nil, 500 })
	st := newTestStore(t)
	ctx := context.Background()
	const room = "wrTESTTMP02"

	if got := newGroupResolver(srv, st, nil).GroupName(ctx, room); got != "" {
		t.Fatalf("临时错误应返回空串，得到 %q", got)
	}
	if _, ok, err := st.GetGroupChat(room); err != nil || ok {
		t.Fatalf("临时错误不应写库: ok=%v err=%v", ok, err)
	}
}

// 群名里的手机号不进文件名。
func TestGroupResolverStripsPhone(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		return groupBody(chatID, "江湾13800000000群"), 0
	})
	if got := newGroupResolver(srv, newTestStore(t), nil).GroupName(context.Background(), "wrTESTPHONE"); got != "江湾群" {
		t.Fatalf("群名应去掉手机号，得到 %q", got)
	}
}

// roomID 为空不调接口。
func TestGroupResolverEmptyRoomID(t *testing.T) {
	g, srv := newGroupStub(t)
	g.setHandler(func(chatID string) (map[string]any, int) {
		t.Errorf("空 roomID 不应调接口")
		return groupBody(chatID, "x"), 0
	})
	if got := newGroupResolver(srv, newTestStore(t), nil).GroupName(context.Background(), ""); got != "" {
		t.Fatalf("空 roomID 应返回空串，得到 %q", got)
	}
	if n := g.callCount(); n != 0 {
		t.Fatalf("不应调接口，实际 %d 次", n)
	}
}

// token 失效（42001）→ 重新 gettoken 后重试成功；错误信息不含 token。
func TestClientGroupChatRetriesOnExpiredToken(t *testing.T) {
	var tokenCalls, groupCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gettoken":
			n := atomic.AddInt32(&tokenCalls, 1)
			writeJSON(t, w, map[string]any{
				"errcode": 0, "errmsg": "ok", "access_token": fmt.Sprintf("tok-%d", n), "expires_in": 7200,
			})
		case "/externalcontact/groupchat/get":
			atomic.AddInt32(&groupCalls, 1)
			if r.URL.Query().Get("access_token") == "tok-1" {
				writeJSON(t, w, map[string]any{"errcode": 42001, "errmsg": "access_token expired"})
				return
			}
			writeJSON(t, w, groupBody("wrTESTTOKEN", "江湾测试群"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	name, err := c.GroupChatName(context.Background(), "wrTESTTOKEN")
	if err != nil {
		t.Fatalf("GroupChatName: %v", err)
	}
	if name != "江湾测试群" {
		t.Fatalf("name=%q", name)
	}
	if tokenCalls != 2 || groupCalls != 2 {
		t.Fatalf("应 gettoken 2 次 / groupchat 2 次，实际 %d / %d", tokenCalls, groupCalls)
	}
}

// 其他非 0 errcode（如 50001）是临时错误：不是 ErrGroupNotFound，错误信息不含 token/URL。
func TestClientGroupChatTempError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gettoken" {
			writeJSON(t, w, map[string]any{"errcode": 0, "errmsg": "ok", "access_token": "SECRET-TOKEN-VALUE", "expires_in": 7200})
			return
		}
		writeJSON(t, w, map[string]any{"errcode": 50001, "errmsg": "some failure"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.GroupChatName(context.Background(), "wrTESTERR")
	if err == nil {
		t.Fatal("期望报错")
	}
	if errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("50001 不应视为 notfound: %v", err)
	}
	msg := err.Error()
	for _, bad := range []string{"SECRET-TOKEN-VALUE", "test-secret", "access_token=", "corpsecret="} {
		if strings.Contains(msg, bad) {
			t.Fatalf("错误信息泄露 %q: %s", bad, msg)
		}
	}
	if !strings.Contains(msg, "errcode=50001") {
		t.Fatalf("错误信息应含 errcode: %s", msg)
	}
}

// 90000-99999 之间的业务错误按 notfound 处理。
func TestClientGroupChatBusinessErrcodeIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gettoken" {
			writeJSON(t, w, map[string]any{"errcode": 0, "errmsg": "ok", "access_token": "tok", "expires_in": 7200})
			return
		}
		writeJSON(t, w, map[string]any{"errcode": 93000, "errmsg": "chat not exist"})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if _, err := c.GroupChatName(context.Background(), "wrTESTGONE"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("期望 ErrGroupNotFound，得到 %v", err)
	}
}
