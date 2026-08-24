//go:build windows

// core_resolve_windows_test.go — resolveCoreEndpoint(prod bootstrap 凭据
// 回退)单测:env 双全 → env;全缺 → 缺省 pipe + state dir secret 文件;
// 半缺/坏 hex → error。
package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCoreEndpointDefaultFallback(t *testing.T) {
	t.Setenv("XNC_DESKTOP_CORE_PIPE", "")
	t.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", "")

	dir := t.TempDir()
	if _, _, err := resolveCoreEndpoint(dir); err == nil {
		t.Fatal("expected error when secret file missing")
	}

	secHex := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	if err := os.WriteFile(filepath.Join(dir, DefaultCoreSecretName),
		[]byte(secHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pipe, secret, err := resolveCoreEndpoint(dir)
	if err != nil {
		t.Fatalf("default fallback: %v", err)
	}
	if pipe != DefaultCorePipe {
		t.Fatalf("pipe = %q, want %q", pipe, DefaultCorePipe)
	}
	if len(secret) != 32 || secret[0] != 1 || secret[31] != 0x20 {
		t.Fatalf("secret decode mismatch (len=%d)", len(secret))
	}
}

func TestResolveCoreEndpointEnvPrecedence(t *testing.T) {
	t.Setenv("XNC_DESKTOP_CORE_PIPE", `\\.\pipe\dev-core`)
	t.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", "aabbccdd")
	// state dir 无文件也不该被读——env 优先。
	pipe, secret, err := resolveCoreEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if pipe != `\\.\pipe\dev-core` || len(secret) != 4 {
		t.Fatalf("env precedence: pipe=%q len=%d", pipe, len(secret))
	}
}

func TestResolveCoreEndpointPartialEnvIsError(t *testing.T) {
	t.Setenv("XNC_DESKTOP_CORE_PIPE", `\\.\pipe\dev-core`)
	t.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultCoreSecretName),
		[]byte("00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCoreEndpoint(dir); err == nil {
		t.Fatal("partially-set env must be an error, not a fallback")
	}
}

func TestNewHandlerDefaultBuildsStarter(t *testing.T) {
	t.Setenv("XNC_DESKTOP_CORE_PIPE", "")
	t.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", "")
	dir := t.TempDir()
	if h := NewHandler(dir, nil); h != nil {
		t.Fatal("expected nil handler when secret file missing")
	}
	secHex := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	if err := os.WriteFile(filepath.Join(dir, DefaultCoreSecretName),
		[]byte(secHex+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(dir, nil)
	if h == nil || h.Starter == nil {
		t.Fatal("expected handler with starter from default endpoint")
	}
	st, ok := h.Starter.(*coreStarter)
	if !ok {
		t.Fatalf("starter type %T", h.Starter)
	}
	if st.pipe != DefaultCorePipe || len(st.secret) != 32 {
		t.Fatalf("starter pipe=%q secretLen=%d", st.pipe, len(st.secret))
	}
}
