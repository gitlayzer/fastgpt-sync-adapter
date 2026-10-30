package wecomcontact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeWeCom 极简企微服务端：gettoken 返回固定 token，externalcontact/get 按配置返回。
type fakeWeCom struct {
	tokenCalls atomic.Int32
	getCalls   atomic.Int32
	token      string // 下发的 access_token
	// getOnce 返回 (body, http status)；可在这里改 token 有效期行为
	get func(w http.ResponseWriter, r *http.Request)
}

func newFakeWeCom(t *testing.T, token string, get func(w http.ResponseWriter, r *http.Request)) (*fakeWeCom, *httptest.Server) {
	t.Helper()
	f := &fakeWeCom{token: token, get: get}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gettoken":
			f.tokenCalls.Add(1)
			if got := r.URL.Query().Get("corpid"); got != "test-corp" {
				t.Errorf("corpid=%q，期望 test-corp", got)
			}
			if got := r.URL.Query().Get("corpsecret"); got != "test-secret" {
				t.Errorf("corpsecret=%q，期望 test-secret", got)
			}
			writeJSON(t, w, map[string]any{
				"errcode": 0, "errmsg": "ok", "access_token": f.token, "expires_in": 7200,
			})
		case "/externalcontact/get":
			f.getCalls.Add(1)
			f.get(w, r)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("写响应: %v", err)
	}
}

func newTestClient(srv *httptest.Server) *Client {
	c := New("test-corp", "test-secret")
	c.SetBaseURL(srv.URL)
	return c
}

// gettoken + get 成功；第二次 Get 复用缓存 token（不再调 gettoken）。
func TestClientGetCachesToken(t *testing.T) {
	f, srv := newFakeWeCom(t, "tok-1", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("access_token"); got != "tok-1" {
			t.Errorf("access_token=%q，期望 tok-1", got)
		}
		if got := r.URL.Query().Get("external_userid"); got != "wmAAAA" {
			t.Errorf("external_userid=%q", got)
		}
		writeJSON(t, w, map[string]any{
			"errcode": 0, "errmsg": "ok",
			"external_contact": map[string]any{
				"external_userid": "wmAAAA", "name": "张三", "type": 2, "corp_name": "某公司",
			},
		})
	})
	c := newTestClient(srv)

	for i := 0; i < 2; i++ {
		got, err := c.Get(context.Background(), "wmAAAA")
		if err != nil {
			t.Fatalf("第 %d 次 Get: %v", i+1, err)
		}
		if got.Name != "张三" || got.CorpName != "某公司" || got.Type != 2 {
			t.Fatalf("Contact 不符: %+v", got)
		}
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("gettoken 应只调 1 次（token 缓存），实际 %d", n)
	}
	if n := f.getCalls.Load(); n != 2 {
		t.Fatalf("externalcontact/get 应调 2 次，实际 %d", n)
	}
}

// get 返回 42001（token 过期）→ 清缓存重新 gettoken 后重试成功。
func TestClientRetriesOnExpiredToken(t *testing.T) {
	var tokenCalls, getCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gettoken":
			n := atomic.AddInt32(&tokenCalls, 1)
			writeJSON(t, w, map[string]any{
				"errcode": 0, "errmsg": "ok", "access_token": fmt.Sprintf("tok-%d", n), "expires_in": 7200,
			})
		case "/externalcontact/get":
			atomic.AddInt32(&getCalls, 1)
			if r.URL.Query().Get("access_token") == "tok-1" {
				writeJSON(t, w, map[string]any{"errcode": 42001, "errmsg": "access_token expired"})
				return
			}
			writeJSON(t, w, map[string]any{
				"errcode": 0, "errmsg": "ok",
				"external_contact": map[string]any{"external_userid": "wmB", "name": "李四", "type": 1},
			})
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.Get(context.Background(), "wmB")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "李四" || got.CorpName != "" || got.Type != 1 {
		t.Fatalf("Contact 不符: %+v", got)
	}
	if tokenCalls != 2 || getCalls != 2 {
		t.Fatalf("应 gettoken 2 次 / get 2 次，实际 %d / %d", tokenCalls, getCalls)
	}
}

// 84061 → ErrNotFound。
func TestClientNotFound(t *testing.T) {
	_, srv := newFakeWeCom(t, "tok-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"errcode": 84061, "errmsg": "not external contact"})
	})
	c := newTestClient(srv)
	_, err := c.Get(context.Background(), "wmGone")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得到 %v", err)
	}
}

// 错误信息里不得出现 access_token / corpsecret。
func TestClientErrorHasNoToken(t *testing.T) {
	_, srv := newFakeWeCom(t, "SECRET-TOKEN-VALUE", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"errcode": 50001, "errmsg": "some failure"})
	})
	c := newTestClient(srv)
	_, err := c.Get(context.Background(), "wmErr")
	if err == nil {
		t.Fatal("期望报错")
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

	// gettoken 失败时同样不能带 secret
	srvTok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"errcode": 40001, "errmsg": "invalid credential"})
	}))
	defer srvTok.Close()
	c2 := newTestClient(srvTok)
	if _, err := c2.Get(context.Background(), "wmErr"); err == nil {
		t.Fatal("gettoken 失败应报错")
	} else if strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("gettoken 错误信息泄露 secret: %v", err)
	}
}
