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
)

// Crypto 企业微信会话内容存档的加解密。
//
// 企微加密流程（每条消息）：
//   1. encrypt_random_key：用企业 RSA 公钥加密的「随机会话密钥」，企业用 RSA 私钥解密得到会话密钥
//   2. encrypt_chat_msg：用会话密钥（AES-GCM）加密的消息体
type Crypto struct {
	privateKey *rsa.PrivateKey
}

// NewCrypto 从 PEM 文本解析 RSA 私钥。
func NewCrypto(privateKeyPEM string) (*Crypto, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("无法解析 RSA 私钥 PEM（请确认是 PKCS#1 或 PKCS#8 格式）")
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
