// cert.go — RTV 腿的 TLS 配置来源。ACME（DNS-01）机制就绪前的形态：
// ① cert/key 文件对（certutil/acme.sh 等外部签发，热加载 = 每次调用重读
// mtime 变化的文件）；② 全缺 = 进程内自签证书（dev-only，启动时高声告警，
// 浏览器 WT 需 --ignore-certificate-errors 或走 WS 兜底；host 腿可用
// --tls-insecure 调试开关）。生产目标形态 = 内嵌 ACME DNS-01（T6）。
package rtv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"os"
	"sync"
	"time"
)

// CertFiles 构造文件形态的 TLS 提供者（certFile/keyFile 须成对）。
func CertFiles(certFile, keyFile string) func([]string) *tls.Config {
	var mu sync.Mutex
	var cached *tls.Certificate
	var lastMod time.Time
	return func(alpn []string) *tls.Config {
		mu.Lock()
		defer mu.Unlock()
		if st, err := os.Stat(certFile); err == nil {
			if cached == nil || !st.ModTime().Equal(lastMod) {
				if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
					cached = &c
					lastMod = st.ModTime()
					slog.Info("rtv: tls cert (re)loaded", "cert", certFile)
				} else {
					slog.Error("rtv: tls cert load failed, keeping previous", "err", err)
				}
			}
		}
		if cached == nil {
			slog.Error("rtv: no tls cert available, leg will fail handshakes", "cert", certFile)
			return &tls.Config{NextProtos: alpn, MinVersion: tls.VersionTLS13}
		}
		return &tls.Config{
			Certificates: []tls.Certificate{*cached},
			NextProtos:   alpn,
			MinVersion:   tls.VersionTLS13,
		}
	}
}

// DevSelfSigned 构造进程内自签证书提供者（dev-only：host 腿调试 /
// --tls-insecure 客户端；浏览器 WT 将拒绝——走 WS 兜底验证路径）。
func DevSelfSigned() func([]string) *tls.Config {
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "xnc-rtv-dev"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	slog.Warn("rtv: using DEV SELF-SIGNED certificate (no XNC_RTV_CERT_FILE/KEY_FILE); browser WT legs will reject it - use the WS fallback or configure production certs")
	return func(alpn []string) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   alpn,
			MinVersion:   tls.VersionTLS13,
		}
	}
}
