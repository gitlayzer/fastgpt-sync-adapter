package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// adminPerson 签名链接里代表管理员通道的 person 值；带 * 是为了不和任何企微 userid 撞（userid 不允许 *）。
const adminPerson = "*admin"

// errBadLink 签名缺失/被篡改/已过期。
var errBadLink = errors.New("invalid or expired link")

// signLink 生成 /v1/file/download 的查询参数：id、p（员工 ID，admin 为 "*admin"）、exp（Unix 秒）、sig。
// sig = base64url(HMAC-SHA256(secret, id + "\n" + p + "\n" + exp))。
// 链接里不带任何明文 token，泄露链接只泄露单个文件且有时效。
func (s *Server) signLink(id, person string, now time.Time) url.Values {
	exp := now.Add(s.opts.LinkTTL).Unix()
	q := url.Values{}
	q.Set("id", id)
	q.Set("p", person)
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("sig", s.signature(id, person, exp))
	return q
}

// signature 计算签名（base64url 无填充）。
func (s *Server) signature(id, person string, exp int64) string {
	mac := hmac.New(sha256.New, s.opts.SignSecret)
	// person 与 id 之间用换行分隔，避免拼接歧义。
	fmt.Fprintf(mac, "%s\n%s\n%d", id, person, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyLink 校验签名与过期；返回 person。比较用 hmac.Equal。
func (s *Server) verifyLink(q url.Values, now time.Time) (string, error) {
	id := q.Get("id")
	person := q.Get("p")
	expStr := q.Get("exp")
	sig := q.Get("sig")
	if id == "" || person == "" || expStr == "" || sig == "" {
		return "", errBadLink
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", errBadLink
	}
	want := s.signature(id, person, exp)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return "", errBadLink
	}
	if now.Unix() > exp {
		return "", errBadLink
	}
	return person, nil
}
