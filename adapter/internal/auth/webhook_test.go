package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fastgpt-sync-adapter/adapter/internal/feishu"
	"fastgpt-sync-adapter/adapter/internal/storage"
)

func newTestService(t *testing.T) (*Service, *storage.Store) {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// app 凭证是空的没关系——status/start 不发请求
	svc := New(Config{Addr: ":0", ExternalURL: "https://adapter.example.com"}, feishu.New("cli_test", "secret"), st)
	return svc, st
}

func TestStartRedirectsToFeishu(t *testing.T) {
	svc, _ := newTestService(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/feishu/start", nil)
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("期望 302, 得到 %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "accounts.feishu.cn/open-apis/authen/v1/authorize") {
		t.Fatalf("跳转地址不对: %s", loc)
	}
	if !strings.Contains(loc, "redirect_uri=https%3A%2F%2Fadapter.example.com%2Foauth%2Ffeishu%2Fcallback") {
		t.Fatalf("redirect_uri 不对: %s", loc)
	}
}

func TestStartShowLink(t *testing.T) {
	svc, _ := newTestService(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/feishu/start?show=1", nil)
	svc.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "accounts.feishu.cn") {
		t.Fatalf("show=1 应返回文本链接: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStatusEmptyThenSeeded(t *testing.T) {
	svc, st := newTestService(t)

	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/status", nil))
	var out struct {
		Count  int `json:"count"`
		Grants []struct {
			UserID string `json:"user_id"`
			Valid  bool   `json:"token_valid_now"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 status: %v", err)
	}
	if out.Count != 0 {
		t.Fatalf("初始应为 0, 得到 %d", out.Count)
	}

	// 塞一条有效授权
	st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_zhangsan",
		AccessToken: "at", RefreshToken: "rt",
		ExpireAt: time.Now().Unix() + 3600, RefreshExpireAt: time.Now().Unix() + 86400,
		Status: "authorized",
	})
	rec2 := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec2, httptest.NewRequest("GET", "/oauth/status", nil))
	var out2 struct {
		Count  int `json:"count"`
		Grants []struct {
			UserID string `json:"user_id"`
			Valid  bool   `json:"token_valid_now"`
		} `json:"grants"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &out2)
	if out2.Count != 1 || out2.Grants[0].UserID != "ou_zhangsan" || !out2.Grants[0].Valid {
		t.Fatalf("status 不对: %+v", out2)
	}
}

func TestCallbackMissingCode(t *testing.T) {
	svc, _ := newTestService(t)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/feishu/callback", nil))
	if rec.Code != 400 {
		t.Fatalf("缺 code 应 400, 得到 %d", rec.Code)
	}
}
