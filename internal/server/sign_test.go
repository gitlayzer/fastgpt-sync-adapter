package server

import (
	"net/url"
	"testing"
	"time"
)

// newSignServer 构造仅用于签名测试的 Server（不涉及鉴权/后端）。
func newSignServer(t *testing.T, ttl time.Duration) *Server {
	t.Helper()
	return New(Options{
		SignSecret: []byte(signTestSecret),
		LinkTTL:    ttl,
	}, func(string) Backend { return nil })
}

// signLink → verifyLink 往返。
func TestSignVerifyRoundTrip(t *testing.T) {
	s := newSignServer(t, 5*time.Minute)
	now := time.Unix(1_700_000_000, 0)

	q := s.signLink("id-abc", "wz", now)
	for _, k := range []string{"id", "p", "exp", "sig"} {
		if q.Get(k) == "" {
			t.Fatalf("signLink 缺少参数 %q: %v", k, q)
		}
	}
	if got, want := q.Get("p"), "wz"; got != want {
		t.Fatalf("p = %q，期望 %q", got, want)
	}
	if got, want := q.Get("exp"), "1700000300"; got != want {
		t.Fatalf("exp = %q，期望 %q", got, want)
	}
	if got := q.Get("id"); got != "id-abc" {
		t.Fatalf("id = %q", got)
	}

	person, err := s.verifyLink(q, now)
	if err != nil {
		t.Fatalf("verifyLink 失败: %v", err)
	}
	if person != "wz" {
		t.Fatalf("person = %q，期望 wz", person)
	}

	// 链接里不应出现任何明文 token 形态的东西
	for _, v := range q {
		if len(v) > 0 && v[0] == signTestSecret {
			t.Fatalf("签名参数里出现了密钥")
		}
	}
}

// 篡改任一字段都必须失败。
func TestVerifyLinkTampered(t *testing.T) {
	s := newSignServer(t, 5*time.Minute)
	now := time.Unix(1_700_000_000, 0)
	base := s.signLink("id-abc", "wz", now)

	cases := map[string]func(url.Values){
		"id":     func(q url.Values) { q.Set("id", "id-other") },
		"p":      func(q url.Values) { q.Set("p", "wl") },
		"exp":    func(q url.Values) { q.Set("exp", "9999999999") },
		"sig":    func(q url.Values) { q.Set("sig", flipFirstChar(q.Get("sig"))) },
		"sig空":   func(q url.Values) { q.Del("sig") },
		"id空":    func(q url.Values) { q.Del("id") },
		"p空":     func(q url.Values) { q.Del("p") },
		"exp非法":  func(q url.Values) { q.Set("exp", "not-a-number") },
		"exp空":   func(q url.Values) { q.Del("exp") },
		"sig换人":  func(q url.Values) { q.Set("sig", s.signLink("id-abc", "wl", now).Get("sig")) },
		"sig换id": func(q url.Values) { q.Set("sig", s.signLink("id-xxx", "wz", now).Get("sig")) },
	}
	for name, mutate := range cases {
		q := cloneValues(base)
		mutate(q)
		if person, err := s.verifyLink(q, now); err == nil {
			t.Fatalf("%s 篡改后仍通过校验（person=%q）", name, person)
		}
	}

	// 未篡改的原始链接仍可通过（确认上面的失败不是由别的原因造成）
	if _, err := s.verifyLink(cloneValues(base), now); err != nil {
		t.Fatalf("原始链接校验失败: %v", err)
	}
}

// 过期失败（以及恰好到期的边界）。
func TestVerifyLinkExpired(t *testing.T) {
	s := newSignServer(t, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	q := s.signLink("id-abc", "wz", now)

	// 未到期
	if _, err := s.verifyLink(q, now); err != nil {
		t.Fatalf("未到期却校验失败: %v", err)
	}
	// 有效期内 +59s
	if _, err := s.verifyLink(q, now.Add(59*time.Second)); err != nil {
		t.Fatalf("有效期内却校验失败: %v", err)
	}
	// 到期那一刻仍可（now == exp）
	if _, err := s.verifyLink(q, now.Add(time.Minute)); err != nil {
		t.Fatalf("正好到期时校验失败: %v", err)
	}
	// 过期
	if _, err := s.verifyLink(q, now.Add(time.Minute+time.Second)); err == nil {
		t.Fatalf("已过期却通过校验")
	}
}

// 换了密钥后旧链接失效；LinkTTL=0 时用默认 10 分钟。
func TestSignSecretAndTTLDefaults(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := newSignServer(t, 0)
	q := a.signLink("id-abc", "wz", now)

	if got, want := q.Get("exp"), "1700000600"; got != want {
		t.Fatalf("默认 TTL 的 exp = %q，期望 %q（10 分钟）", got, want)
	}
	if len(a.opts.SignSecret) < 32 {
		t.Fatalf("未配置 SignSecret 时应自动生成 >=32 字节，实际 %d", len(a.opts.SignSecret))
	}

	b := New(Options{SignSecret: []byte("another-secret-another-secret-32"), LinkTTL: time.Minute},
		func(string) Backend { return nil })
	if _, err := b.verifyLink(q, now); err == nil {
		t.Fatalf("换密钥后旧签名仍通过校验")
	}
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		c := make([]string, len(vs))
		copy(c, vs)
		out[k] = c
	}
	return out
}

func flipFirstChar(s string) string {
	if s == "" {
		return "a"
	}
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}
