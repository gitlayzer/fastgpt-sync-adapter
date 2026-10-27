package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
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

// stubTransport 拦截飞书请求，返回固定响应。
type stubTransport func(*http.Request) (*http.Response, error)

func (f stubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// seedExpiringGrant 塞一条 access_token 即将过期、会被本轮刷新的授权，并让刷新接口返回 body。
func seedExpiringGrant(t *testing.T, svc *Service, st *storage.Store, httpStatus int, body string) {
	t.Helper()
	svc.fs.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/authen/v2/oauth/token") {
			t.Fatalf("意外请求: %s", r.URL)
		}
		return &http.Response{StatusCode: httpStatus, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	now := time.Now().Unix()
	if err := st.UpsertOAuthGrant(&storage.OAuthGrant{
		Platform: "feishu", ExternalUserID: "ou_zhangsan",
		AccessToken: "at", RefreshToken: "rt",
		ExpireAt: now + 60, RefreshExpireAt: now + 86400,
		Status: "authorized",
	}); err != nil {
		t.Fatalf("UpsertOAuthGrant: %v", err)
	}
}

func TestRefreshReauthCodeMarksExpired(t *testing.T) {
	for _, code := range []string{"20064", "20073", "20037", "20066"} {
		t.Run(code, func(t *testing.T) {
			svc, st := newTestService(t)
			seedExpiringGrant(t, svc, st, 400, `{"code":`+code+`,"error":"invalid_grant","error_description":"x"}`)
			if n := svc.refreshExpiring(); n != 0 {
				t.Fatalf("刷新失败不应计入成功数, 得到 %d", n)
			}
			g, err := st.GetOAuthGrant("feishu", "ou_zhangsan")
			if err != nil {
				t.Fatalf("GetOAuthGrant: %v", err)
			}
			if g.Status != "expired" {
				t.Fatalf("code=%s 应标 expired, 得到 %s", code, g.Status)
			}
		})
	}
}

func TestRefreshAppLevelErrorKeepsAuthorized(t *testing.T) {
	svc, st := newTestService(t)
	// 20074：应用没开刷新开关，是配置问题，不能把用户标失效
	seedExpiringGrant(t, svc, st, 400, `{"code":20074,"error":"invalid_request","error_description":"x"}`)
	svc.refreshExpiring()
	g, err := st.GetOAuthGrant("feishu", "ou_zhangsan")
	if err != nil {
		t.Fatalf("GetOAuthGrant: %v", err)
	}
	if g.Status != "authorized" {
		t.Fatalf("应用级错误不应改状态, 得到 %s", g.Status)
	}
}

func TestRefreshSuccessUpdatesToken(t *testing.T) {
	svc, st := newTestService(t)
	seedExpiringGrant(t, svc, st, 200, `{"code":0,"access_token":"at2","refresh_token":"rt2","expires_in":7200,"refresh_token_expires_in":604800}`)
	if n := svc.refreshExpiring(); n != 1 {
		t.Fatalf("应刷新 1 条, 得到 %d", n)
	}
	g, err := st.GetOAuthGrant("feishu", "ou_zhangsan")
	if err != nil {
		t.Fatalf("GetOAuthGrant: %v", err)
	}
	if g.Status != "authorized" || g.AccessToken != "at2" || g.RefreshToken != "rt2" {
		t.Fatalf("刷新结果不对: status=%s at=%s rt=%s", g.Status, g.AccessToken, g.RefreshToken)
	}
}

// 拿不到用户信息时授权失败，不保存以假 ID（unknown_…）开头的授权。
func TestCallbackUserInfoFailNotSaved(t *testing.T) {
	svc, st := newTestService(t)
	svc.fs.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"access_token":"at","refresh_token":"rt","expires_in":7200,"refresh_token_expires_in":604800}`
		if strings.HasSuffix(r.URL.Path, "/authen/v1/user_info") {
			body = `{"code":99991663,"msg":"invalid access token"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/feishu/callback?code=c1", nil))
	if rec.Code == 200 {
		t.Fatalf("拿不到用户信息时不应返回授权成功")
	}
	grants, err := st.ListOAuthGrants("feishu")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("不应保存授权，实际 %d 条（%s）", len(grants), grants[0].ExternalUserID)
	}
}
