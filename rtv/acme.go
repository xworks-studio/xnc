// acme.go — RTV 腿的 ACME DNS-01 证书管理（Let's Encrypt + Aliyun DNS）。
// 浏览器 WT 走标准 Web PKI（真实 CA 证书，无 serverCertificateHashes 层）；
// host 腿同用该证书（rustls 系统根校验）。HTTP-01/TLS-ALPN 不可用（TCP443
// 归 caddy），DNS-01 是唯一通路——凭据 ALIDNS_ACCESS_KEY/SECRET（deploy/.env
// 唯一源，严禁入库/日志）。
//
// 形态：在 certDir 内维护 <domain>.crt / <domain>.key（PEM）。启动时已有
// 且剩余有效期 >30 天 → 直接复用；否则签发/续期。每日巡检一次（<30 天重
// 签）。落盘后 rtv.CertFiles 按 mtime 热加载——续期换证会断开既有 QUIC
// 连接，host/viewer 的自动重连语义兜底。
package rtv

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/alidns"
	"github.com/go-acme/lego/v4/registration"
)

// newAlidnsProvider 构造 Aliyun DNS-01 provider（RAM 凭据，绝不入日志）。
func newAlidnsProvider(key, secret string) (challenge.Provider, error) {
	return alidns.NewDNSProviderConfig(&alidns.Config{APIKey: key, SecretKey: secret})
}

// ACMEConfig DNS-01 签发配置（源自 server config）。
type ACMEConfig struct {
	Dir          string // 证书存储目录（卷持久化；账号密钥同存于此）
	Domain       string // xnc.app（SAN 单域）
	Email        string // ACME 账户邮箱
	AlidnsKey    string // ALIDNS_ACCESS_KEY
	AlidnsSecret string // ALIDNS_SECRET_KEY
	Staging      bool   // true = LE staging（联调用，防配额烧穿）
}

// acmeUser lego 账户（注册信息在 Register 后回填）。
type acmeUser struct {
	email string
	reg   *registration.Resource
	key   crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// StartACME 确保 certDir 内存在有效证书并启动每日续期巡检；返回
// (certFile, keyFile)（rtv.CertFiles 的输入）。失败即返（调用方决定
// 是否回退 dev 自签——生产应当 fail fast）。
func StartACME(cfg ACMEConfig) (string, string, error) {
	if cfg.Dir == "" || cfg.Domain == "" || cfg.Email == "" ||
		cfg.AlidnsKey == "" || cfg.AlidnsSecret == "" {
		return "", "", fmt.Errorf("rtv: acme disabled (dir/domain/email/alidns credentials required)")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return "", "", fmt.Errorf("rtv: acme cert dir: %w", err)
	}
	certFile := filepath.Join(cfg.Dir, cfg.Domain+".crt")
	keyFile := filepath.Join(cfg.Dir, cfg.Domain+".key")

	ensure := func() error { return ensureCert(cfg, certFile, keyFile) }
	if err := ensure(); err != nil {
		return "", "", err
	}
	go func() {
		for {
			time.Sleep(24 * time.Hour)
			if err := ensure(); err != nil {
				slog.Error("rtv: acme renewal check failed", "err", err)
			}
		}
	}()
	return certFile, keyFile, nil
}

// certNeedsRenewal 解析 PEM 证书链，剩余有效期 < 30 天即需续期。
func certNeedsRenewal(certFile string) (bool, error) {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return true, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return true, fmt.Errorf("rtv: bad cert pem")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return true, err
	}
	return time.Until(leaf.NotAfter) < 30*24*time.Hour, nil
}

func ensureCert(cfg ACMEConfig, certFile, keyFile string) error {
	if _, err := os.Stat(certFile); err == nil {
		need, nerr := certNeedsRenewal(certFile)
		if nerr != nil {
			slog.Warn("rtv: acme existing cert unreadable, re-obtaining", "err", nerr)
		} else if !need {
			return nil
		}
	}

	// 账户密钥（持久化：复用 ACME 账户，防每轮注册新账户触发限频）。
	accountKeyPath := filepath.Join(cfg.Dir, "account.key")
	key, err := loadOrCreateKey(accountKeyPath)
	if err != nil {
		return err
	}
	user := &acmeUser{email: cfg.Email, key: key}

	lc := lego.NewConfig(user)
	if cfg.Staging {
		lc.CADirURL = lego.LEDirectoryStaging
	}
	client, err := lego.NewClient(lc)
	if err != nil {
		return fmt.Errorf("rtv: acme client: %w", err)
	}
	// DNS-01：Aliyun DNS（凭据绝不入日志）。
	prov, err := newAlidnsProvider(cfg.AlidnsKey, cfg.AlidnsSecret)
	if err != nil {
		return fmt.Errorf("rtv: alidns provider: %w", err)
	}
	if err := client.Challenge.SetDNS01Provider(prov); err != nil {
		return fmt.Errorf("rtv: set dns01: %w", err)
	}

	if user.reg == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{
			TermsOfServiceAgreed: true,
		})
		if err != nil {
			return fmt.Errorf("rtv: acme register: %w", err)
		}
		user.reg = reg
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{cfg.Domain},
		Bundle:  true,
	})
	if err != nil {
		return fmt.Errorf("rtv: acme obtain: %w", err)
	}
	tmp := certFile + ".tmp"
	if err := os.WriteFile(tmp, res.Certificate, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, certFile); err != nil {
		return err
	}
	tmpK := keyFile + ".tmp"
	if err := os.WriteFile(tmpK, res.PrivateKey, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpK, keyFile); err != nil {
		return err
	}
	slog.Info("rtv: acme cert obtained/renewed", "domain", cfg.Domain,
		"staging", cfg.Staging)
	return nil
}

func loadOrCreateKey(path string) (crypto.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk != nil && blk.Type == "EC PRIVATE KEY" {
			return x509.ParseECPrivateKey(blk.Bytes)
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: der,
	}), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}
