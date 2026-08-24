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
	clearCoreEnv(t)

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
	clearCoreEnv(t)
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

// legacy shellhost 链(XNC_CORE_*)优先于 canonical 链(XNC_DESKTOP_CORE_*),
// 与旧 DefaultShellHost 语义一致(2026-08-24 follow-up Task 7c 合并)。
func TestResolveCoreEndpointLegacyChainPriority(t *testing.T) {
	clearCoreEnv(t)
	t.Setenv("XNC_CORE_PIPE", `\\.\pipe\legacy-core`)
	t.Setenv("XNC_CORE_SECRET_HEX", "11223344")
	t.Setenv("XNC_DESKTOP_CORE_PIPE", `\\.\pipe\dev-core`)
	t.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", "aabbccdd")
	pipe, secret, err := resolveCoreEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if pipe != `\\.\pipe\legacy-core` || len(secret) != 4 {
		t.Fatalf("legacy chain must win: pipe=%q len=%d", pipe, len(secret))
	}
}

// legacy 链半缺 → 回落 canonical 链(保持旧 DefaultShellHost 的回落行为)。
func TestResolveCoreEndpointLegacyPartialFallsThrough(t *testing.T) {
	clearCoreEnv(t)
	t.Setenv("XNC_CORE_PIPE", `\\.\pipe\legacy-core`) // secret 缺失 = 半缺
	t.Setenv("XNC_DESKTOP_CORE_PIPE", `\\.\pipe\dev-core`)
	t.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", "aabbccdd")
	pipe, secret, err := resolveCoreEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if pipe != `\\.\pipe\dev-core` || len(secret) != 4 {
		t.Fatalf("partial legacy chain must fall through: pipe=%q len=%d", pipe, len(secret))
	}
}

// DefaultShellHost 形态(stateDir 为空)不读 cwd 下的 secret 文件。
func TestResolveCoreEndpointEmptyStateDirNoFileFallback(t *testing.T) {
	clearCoreEnv(t)
	if _, _, err := resolveCoreEndpoint(""); err == nil {
		t.Fatal("empty stateDir must error, not read the cwd for a secret file")
	}
}

// clearCoreEnv 清空两条凭据 env 链(防宿主环境残留干扰)。
func clearCoreEnv(t *testing.T) {
	for _, k := range []string{
		"XNC_CORE_PIPE", "XNC_CORE_SECRET_HEX",
		"XNC_DESKTOP_CORE_PIPE", "XNC_DESKTOP_CORE_SECRET_HEX",
	} {
		t.Setenv(k, "")
	}
}

func TestResolveCoreEndpointPartialEnvIsError(t *testing.T) {
	clearCoreEnv(t)
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
	clearCoreEnv(t)
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
