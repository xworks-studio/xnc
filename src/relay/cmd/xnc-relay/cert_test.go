// cert_test.go — certManager 轮转语义与 endpointConfig 候选顺序
// （2026-09-16 r1 证书过期事故的回归覆盖）。
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xnc/proto"
)

func TestCertManagerRotateChangesSHAAndNotifies(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := newCertManager(ctx, dir)
	sha1 := m.SHA()
	if sha1 == "" {
		t.Fatal("initial SHA empty")
	}
	ch := m.Changed()

	// 落盘的证书对可加载且指纹一致。
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "wt-cert.pem"), filepath.Join(dir, "wt-key.pem"))
	if err != nil {
		t.Fatalf("load generated pair: %v", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	if got := hex.EncodeToString(sum[:]); got != sha1 {
		t.Fatalf("disk fingerprint %s != SHA() %s", got, sha1)
	}
	if exp := m.expiresIn(); exp < 6*24*time.Hour {
		t.Fatalf("fresh cert expiresIn = %v, want ≈7d", exp)
	}

	// 轮转:指纹变化 + 通知通道关闭 + Changed() 换新通道。
	m.rotate()
	sha2 := m.SHA()
	if sha2 == "" || sha2 == sha1 {
		t.Fatalf("rotate must change SHA (before=%s after=%s)", sha1, sha2)
	}
	select {
	case <-ch:
	default:
		t.Fatal("rotated channel must be closed")
	}
	select {
	case <-m.Changed():
		t.Fatal("new Changed channel must be open")
	default:
	}
}

func TestCertManagerLoadsValidDiskCert(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	m1 := newCertManager(ctx, dir)
	want := m1.SHA()
	cancel()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m2 := newCertManager(ctx2, dir)
	if m2.SHA() != want {
		t.Fatalf("restart must reuse disk cert: %s != %s", m2.SHA(), want)
	}
}

func TestCertManagerFreshLoadSkipsNearExpiry(t *testing.T) {
	// 间接验证:生成后手动把磁盘证书视作"临期"不可行(有效期在证书里),
	// 这里验证新证书剩余 > 阈值时不会在构造即轮转 Changed 通道(即走
	// 加载路径而非 rotate 路径)。
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newCertManager(ctx, dir)
	select {
	case <-m.Changed():
		t.Fatal("fresh load path must not fire Changed (rotate-only signal)")
	default:
	}
	if _, err := os.Stat(filepath.Join(dir, "wt-cert.pem")); err != nil {
		t.Fatalf("cert persisted: %v", err)
	}
	// 证书有效期内、SAN/CN 形态保持(CN=xnc-relay)。
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "wt-cert.pem"), filepath.Join(dir, "wt-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if leaf.Subject.CommonName != "xnc-relay" {
		t.Fatalf("CN = %q, want xnc-relay", leaf.Subject.CommonName)
	}
}

func TestEndpointConfigOrder(t *testing.T) {
	shaFn := func() string { return "abc" }
	// 域名形态:wt → quic → ws(域名) → ws(IP) → sdata。
	eps := endpointConfig{
		publicHost: "1.2.3.4", wtPort: 443, hostPort: 4433, wsPort: 443,
		sessionHost: "r1.example", sessionPort: 443, httpLeg: true, certSHA: shaFn,
	}.build()
	got := make([]string, len(eps))
	for i, e := range eps {
		got[i] = e.Transport + ":" + e.Host
	}
	want := []string{"wt:1.2.3.4", "quic:1.2.3.4", "ws:r1.example", "ws:1.2.3.4", "sdata:r1.example"}
	if len(got) != len(want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoint[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
	if eps[0].CertSHA256 != "abc" {
		t.Fatalf("wt candidate must carry certSHA, got %q", eps[0].CertSHA256)
	}

	// 无域名:无 sdata、无域名 ws;无 HTTP 腿:只有 wt+quic。
	eps2 := endpointConfig{
		publicHost: "1.2.3.4", wtPort: 443, hostPort: 4433, wsPort: 443,
		httpLeg: true, certSHA: shaFn,
	}.build()
	if len(eps2) != 3 || eps2[2].Host != "1.2.3.4" {
		t.Fatalf("no-domain endpoints = %v", eps2)
	}
	eps3 := endpointConfig{
		publicHost: "1.2.3.4", wtPort: 443, hostPort: 4433, certSHA: shaFn,
	}.build()
	if len(eps3) != 2 {
		t.Fatalf("no-http-leg endpoints = %v", eps3)
	}
}

func TestEndpointConfigCertSHADynamic(t *testing.T) {
	// 指纹经闭包实时取:轮转后 build() 即带新值(server 侧重连重注册
	// 的路径保证)。
	cur := "old"
	eps := endpointConfig{publicHost: "1.2.3.4", wtPort: 443, hostPort: 4433,
		httpLeg: false, certSHA: func() string { return cur }}.build()
	if eps[0].CertSHA256 != "old" {
		t.Fatalf("want old, got %s", eps[0].CertSHA256)
	}
	cur = "new"
	if eps = (endpointConfig{publicHost: "1.2.3.4", wtPort: 443, hostPort: 4433,
		httpLeg: false, certSHA: func() string { return cur }}).build(); eps[0].CertSHA256 != "new" {
		t.Fatalf("want new, got %s", eps[0].CertSHA256)
	}
	_ = proto.EndpointDesc{} // keep import if unused above
}
