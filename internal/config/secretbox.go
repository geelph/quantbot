// Package config 配置管理：密钥加密（AES-GCM）。
// 对应 AlphaLom 的 utils.secret_box 加密方案——LLM 平台 API Key 等敏感字段
// 在 config.json 里以密文存储，运行时用主密钥解密。
//
// 主密钥来源（优先级从高到低）：
//  1) 环境变量 QUANTBOT_SECRET_KEY（base64 编码的 32 字节 key）
//  2) config.json 的 master_key 字段（明文 32 字节 key，base64）
//  3) 内置开发密钥（dev-only，生产务必覆盖）
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"sync"
)

var (
	// errNoKey 主密钥未配置，无法解密
	errNoKey = errors.New("config: secret key not configured")
	// devKey 内置开发密钥（生产务必用环境变量覆盖）
	devKey = []byte("QuantBot-Dev-Secret-Key-32Byte!!")
)

type secretBox struct {
	mu  sync.Once
	key []byte
}

var globalBox = &secretBox{}

func (s *secretBox) init(keyFromConfig []byte) error {
	s.mu.Do(func() {
		if v := os.Getenv("QUANTBOT_SECRET_KEY"); v != "" {
			if decoded, err := base64.StdEncoding.DecodeString(v); err == nil && len(decoded) == 32 {
				s.key = decoded
				return
			}
			// 环境变量非 base64 或长度不对，回退 config / devKey
		}
		if len(keyFromConfig) == 32 {
			s.key = keyFromConfig
			return
		}
		s.key = devKey
	})
	if len(s.key) == 0 {
		return errNoKey
	}
	return nil
}

// Encrypt 明文 → AES-GCM 密文（base64 输出）。
// keyFromConfig 是 config.json 里配置的主密钥（可为 nil，使用 env 或内置）。
func Encrypt(plaintext string, keyFromConfig []byte) (string, error) {
	if err := globalBox.init(keyFromConfig); err != nil {
		return "", err
	}
	block, err := aes.NewCipher(globalBox.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt AES-GCM 密文（base64） → 明文。
func Decrypt(ciphertext string, keyFromConfig []byte) (string, error) {
	if err := globalBox.init(keyFromConfig); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(globalBox.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("config: ciphertext too short")
	}
	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
