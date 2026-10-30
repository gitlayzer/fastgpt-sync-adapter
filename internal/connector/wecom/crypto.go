package wecom

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"regexp"
)

// Crypto 企业微信会话内容存档的加解密。
//
// 企微加密流程（每条消息）：
//  1. encrypt_random_key：用企业 RSA 公钥加密的「随机会话密钥」，企业用 RSA 私钥解密得到会话密钥
//  2. encrypt_chat_msg：用会话密钥（AES-GCM）加密的消息体
type Crypto struct {
	privateKey *rsa.PrivateKey
}

// NewCrypto 从 PEM 文本解析 RSA 私钥。
func NewCrypto(privateKeyPEM string) (*Crypto, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		// pem.Decode 返回 nil 最常见的原因不是格式，而是 PEM 结构本身不完整：
		// 首尾标签不一致（改标签只改了一行）、缺 END 行、复制时丢了换行。
		// 这里先做一次针对性检查，避免把用户引到「格式不对」的岔路上。
		if msg := checkPEMShape(privateKeyPEM); msg != "" {
			return nil, fmt.Errorf("无法解析 RSA 私钥 PEM: %s", msg)
		}
		return nil, fmt.Errorf("无法解析 RSA 私钥 PEM（未找到完整的 BEGIN/END 块，请确认是 PKCS#1 或 PKCS#8 格式）")
	}
	// 先试 PKCS#1
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &Crypto{privateKey: key}, nil
	}
	// 再试 PKCS#8
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败（既非 PKCS#1 也非 PKCS#8）: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥不是 RSA 类型")
	}
	return &Crypto{privateKey: rsaKey}, nil
}

// checkPEMShape 在 pem.Decode 失败后给出更具体的原因。
// 返回空串表示没查出明显问题（交给调用方给通用提示）。
func checkPEMShape(s string) string {
	beginRe := regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]+)-----`)
	endRe := regexp.MustCompile(`-----END ([A-Z0-9 ]+)-----`)
	b := beginRe.FindStringSubmatch(s)
	e := endRe.FindStringSubmatch(s)
	switch {
	case b == nil && e == nil:
		return "文件内容里没有 BEGIN/END 行，确认是不是把 .pem 的内容贴成了别的文件"
	case b == nil:
		return fmt.Sprintf("缺少 -----BEGIN ...----- 行（找到 END %s）", e[1])
	case e == nil:
		return fmt.Sprintf("缺少 -----END %s----- 行，文件可能被截断或复制不完整", b[1])
	case b[1] != e[1]:
		return fmt.Sprintf("首尾标签不一致：BEGIN %s 对 END %s，两行必须同名", b[1], e[1])
	}
	return ""
}

// DecryptRandomKey 用 RSA 私钥解密 encrypt_random_key，得到 base64 编码的会话密钥。
// 企微用的是 RSA PKCS#1 v1.5 填充（部分场景 OAEP，这里两种都尝试）。
func (c *Crypto) DecryptRandomKey(encryptRandomKeyB64 string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encryptRandomKeyB64)
	if err != nil {
		return "", fmt.Errorf("random_key base64 解码失败: %w", err)
	}
	// 先试 PKCS#1 v1.5（企微官方文档默认）
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, c.privateKey, ciphertext)
	if err != nil {
		// 退而试 OAEP
		plain, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, c.privateKey, ciphertext, nil)
		if err != nil {
			return "", fmt.Errorf("RSA 解密 random_key 失败: %w", err)
		}
	}
	// 企微返回的会话密钥本身是 base64 字符串
	return string(plain), nil
}

// DecryptChatMsg 用会话密钥（AES-GCM）解密 encrypt_chat_msg。
// sessionKeyB64 是 DecryptRandomKey 的结果（base64）。
func (c *Crypto) DecryptChatMsg(sessionKeyB64, encryptChatMsgB64 string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(sessionKeyB64)
	if err != nil {
		return "", fmt.Errorf("会话密钥 base64 解码失败: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encryptChatMsgB64)
	if err != nil {
		return "", fmt.Errorf("消息体 base64 解码失败: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("AES 初始化失败: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("GCM 初始化失败: %w", err)
	}
	// 企微密文 = nonce(12B) + ciphertext+tag
	ns := aesgcm.NonceSize()
	if len(ciphertext) < ns {
		return "", fmt.Errorf("密文长度不足（%d < nonce %d）", len(ciphertext), ns)
	}
	nonce, body := ciphertext[:ns], ciphertext[ns:]
	plain, err := aesgcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("AES-GCM 解密失败: %w", err)
	}
	return string(plain), nil
}

// DecryptMessage 一步完成：先解 random_key 得会话密钥，再解消息体。
func (c *Crypto) DecryptMessage(encryptRandomKeyB64, encryptChatMsgB64 string) (string, error) {
	sessionKey, err := c.DecryptRandomKey(encryptRandomKeyB64)
	if err != nil {
		return "", err
	}
	return c.DecryptChatMsg(sessionKey, encryptChatMsgB64)
}
