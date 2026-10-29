package synology

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 断言脱敏后的错误里既没有原始密码（编码前后的形式）也没有账号名，
// 并且确实出现了 passwd 的 *** 占位。
func assertRedacted(t *testing.T, err error, secret string, containsAccount bool) string {
	t.Helper()
	if err == nil {
		t.Fatalf("redactErr 返回 nil，期望非 nil")
	}
	msg := err.Error()
	for _, bad := range []string{secret, "S3cret%40pw", "S3cret@pw"} {
		if bad == "" {
			continue
		}
		if strings.Contains(msg, bad) {
			t.Errorf("脱敏后的错误仍包含敏感内容 %q: %s", bad, msg)
		}
	}
	if containsAccount && strings.Contains(msg, "nasuser") {
		t.Errorf("脱敏后的错误仍包含账号名: %s", msg)
	}
	if !strings.Contains(msg, "passwd=%2A%2A%2A") && !strings.Contains(msg, "passwd=***") {
		t.Errorf("脱敏后的错误缺少 passwd 占位: %s", msg)
	}
	return msg
}

func TestRedactErrURLError(t *testing.T) {
	const rawURL = "https://nas:5001/webapi/auth.cgi?api=SYNO.API.Auth&account=nasuser&passwd=S3cret%40pw&format=sid"
	e := &url.Error{Op: "Get", URL: rawURL, Err: io.EOF}

	got := redactErr(e)
	msg := assertRedacted(t, got, "S3cret%40pw", true)

	if !errors.Is(got, io.EOF) {
		t.Errorf("errors.Is(redactErr(err), io.EOF) 应为 true，实际 false: %v", got)
	}
	if e.URL != rawURL {
		t.Errorf("原 *url.Error 的 URL 被修改了: %s", e.URL)
	}
	if !strings.Contains(msg, "SYNO.API.Auth") {
		t.Errorf("非敏感参数不应丢失: %s", msg)
	}
	if !strings.Contains(msg, "format=sid") {
		t.Errorf("非敏感参数不应丢失: %s", msg)
	}
}

func TestRedactErrSid(t *testing.T) {
	const rawURL = "https://nas:5001/webapi/entry.cgi?api=SYNO.FileStation.Download&_sid=abc123&method=download&version=2"
	e := &url.Error{Op: "Get", URL: rawURL, Err: io.EOF}

	got := redactErr(e)
	msg := got.Error()
	if strings.Contains(msg, "abc123") {
		t.Errorf("脱敏后的错误仍包含 sid: %s", msg)
	}
	if !strings.Contains(msg, "_sid=%2A%2A%2A") && !strings.Contains(msg, "_sid=***") {
		t.Errorf("脱敏后的错误缺少 _sid 占位: %s", msg)
	}
	if !errors.Is(got, io.EOF) {
		t.Errorf("errors.Is(redactErr(err), io.EOF) 应为 true")
	}
	if e.URL != rawURL {
		t.Errorf("原 *url.Error 的 URL 被修改了: %s", e.URL)
	}
}

func TestRedactErrNonURL(t *testing.T) {
	plain := errors.New("x")
	if got := redactErr(plain); got != plain {
		t.Errorf("非 *url.Error 应原样返回，got %v", got)
	}
	if got := redactErr(nil); got != nil {
		t.Errorf("nil 应返回 nil，got %v", got)
	}
}

// 覆盖 HTTP 客户端返回真 *url.Error 时的脱敏效果：不能把账号密码带进日志。
func TestLoginErrorRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter 不支持 Hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack 失败: %v", err)
			return
		}
		conn.Close() // 制造 EOF
	}))
	defer srv.Close()

	c := New(srv.URL, "nasuser", "S3cret@pw")
	retryable, err := c.loginOnce()
	if err == nil {
		t.Fatalf("期望登录失败，实际成功")
	}
	msg := err.Error()
	if strings.Contains(msg, "S3cret") {
		t.Errorf("登录错误泄露了账号/密码: %s", msg)
	}
	if strings.Contains(msg, "321") {
		t.Errorf("登录错误泄露了密码: %s", msg)
	}
	t.Logf("retryable=%v err=%s", retryable, msg)
}
