// Package apitoken 员工专属 API token：生成、哈希与前缀。明文只在签发时出现一次，库里只存哈希。
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Prefix token 明文固定前缀，便于日志和配置里一眼认出来。
const Prefix = "fsa_"

// randBytes 随机数长度：32 字节随机数的 base64url（无填充）正好 43 个字符。
const randBytes = 32

// displayChars Display 在 Prefix 之后保留的字符数。
const displayChars = 6

// Generate 生成一个新 token 明文。
func Generate() (string, error) {
	b := make([]byte, randBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机数: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash 明文 → SHA-256 hex（存库与查库都用它）。
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Display 便于辨认的前缀：Prefix 后再取 6 个字符，如 "fsa_AbC123"；长度不足时原样返回。
func Display(token string) string {
	n := len(Prefix) + displayChars
	if len(token) < n {
		return token
	}
	return token[:n]
}
