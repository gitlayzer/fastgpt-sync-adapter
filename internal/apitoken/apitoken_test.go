package apitoken

import (
	"regexp"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate()
	if err != nil {
		t.Fatalf("Generate 第二次: %v", err)
	}
	if a == b {
		t.Fatalf("两次生成不应相同: %s", a)
	}
	if !strings.HasPrefix(a, Prefix) {
		t.Fatalf("应以 %q 开头: %s", Prefix, a)
	}
	// 4 字符前缀 + 32 字节随机数的 base64url（无填充）= 4 + 43
	if len(a) != 4+43 {
		t.Fatalf("长度应为 %d，实际 %d: %s", 4+43, len(a), a)
	}
}

func TestHash(t *testing.T) {
	token := "fsa_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	h1 := Hash(token)
	if h1 != Hash(token) {
		t.Fatal("同一明文哈希应稳定")
	}
	if len(h1) != 64 {
		t.Fatalf("哈希应为 64 位 hex，实际 %d: %s", len(h1), h1)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h1) {
		t.Fatalf("哈希应为小写 hex: %s", h1)
	}
	if Hash(token+"x") == h1 {
		t.Fatal("不同明文哈希不应相同")
	}
	if strings.Contains(h1, token) {
		t.Fatal("哈希不应包含明文")
	}
}

func TestDisplay(t *testing.T) {
	token := Prefix + "AbC123xyz"
	if got := Display(token); got != "fsa_AbC123" {
		t.Fatalf("Display = %q, 期望 %q", got, "fsa_AbC123")
	}
	// 长度不足时原样返回
	for _, short := range []string{"", "fsa", "fsa_AbC12"} {
		if got := Display(short); got != short {
			t.Fatalf("短 token %q 应原样返回，实际 %q", short, got)
		}
	}
}
