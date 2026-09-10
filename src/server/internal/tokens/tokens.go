// Package tokens 生成并哈希 enrollment token；DB 只存 hex(sha256(plaintext))。
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// Generate 返回 plaintext（"xnc_enroll_" + 43 字符 base64url，32 字节 CSPRNG）
// 与 hash = hex(sha256(plaintext))。
func Generate() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	plaintext = "xnc_enroll_" + base64.RawURLEncoding.EncodeToString(b)
	return plaintext, Hash(plaintext), nil
}

func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
