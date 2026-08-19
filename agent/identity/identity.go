// Package identity 管理节点 Ed25519 身份：生成、签名、落盘（Windows 上经 DPAPI 保护）。
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

var errInvalidKey = errors.New("invalid private key")

type Key struct {
	NodeID string
	Priv   ed25519.PrivateKey
}

// Generate 生成新密钥对。NodeID 为空表示尚未注册。
func Generate() *Key {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_ = pub
	return &Key{Priv: priv}
}

func (k *Key) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(k.Priv.Public().(ed25519.PublicKey))
}

func (k *Key) Sign(data []byte) []byte { return ed25519.Sign(k.Priv, data) }

type stored struct {
	NodeID     string `json:"nodeId"`
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"` // base64(protect(raw 私钥))
}

func Save(k *Key, path string) error {
	raw := make([]byte, ed25519.PrivateKeySize)
	copy(raw, k.Priv)
	blob := protect(raw)
	clear(raw) // 私钥明文副本用完即擦
	s := stored{NodeID: k.NodeID, PublicKey: k.PublicKeyB64(),
		PrivateKey: base64.StdEncoding.EncodeToString(blob)}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func Load(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	blob, err := base64.StdEncoding.DecodeString(s.PrivateKey)
	if err != nil {
		return nil, err
	}
	raw, err := unprotect(blob)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, errInvalidKey
	}
	return &Key{NodeID: s.NodeID, Priv: ed25519.PrivateKey(raw)}, nil
}
