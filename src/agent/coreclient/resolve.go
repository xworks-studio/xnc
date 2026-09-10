//go:build windows

// resolve.go — core 连接凭据解析(自 agent/desktop 迁入,desktop/shellhost
// 共用同一凭据源;单一事实)。
package coreclient

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 生产缺省凭据(prod bootstrap):XNCCore 服务用 --secret-file 持久化
// secret,binPath 固定管道名;agent 无 env/flag 时按同约定回退。
const (
	// DefaultCorePipe XNCCore 服务的固定 XNIP 管道名。
	DefaultCorePipe = `\\.\pipe\xnc-core`
	// DefaultCoreSecretName agent state dir 下的 secret 文件名
	// (hex + 换行,与 xnc-core --secret-file 写盘格式一致)。
	DefaultCoreSecretName = "core-secret.hex"
)

// CoreSecretPath 返回 <stateDir>\core-secret.hex。
func CoreSecretPath(stateDir string) string {
	return filepath.Join(stateDir, DefaultCoreSecretName)
}

// ResolveCoreEndpoint 凭据解析(纯函数,便于单测):
//
//	env 链(逐条双全才可用,按优先级):
//	  XNC_CORE_PIPE + XNC_CORE_SECRET_HEX          (legacy shellhost 链,优先)
//	  XNC_DESKTOP_CORE_PIPE + XNC_DESKTOP_CORE_SECRET_HEX (canonical 链)
//	legacy 链半缺 → 回落 canonical 链(保持旧 DefaultShellHost 语义);
//	canonical 链半缺 / 任一链坏 hex → error(不回退弱配置);
//	env 全缺 → 生产缺省(DefaultCorePipe + CoreSecretPath(stateDir) 读盘,
//	trim 换行);stateDir 为空(dev 形态)不读盘,直接 error。
func ResolveCoreEndpoint(stateDir string) (pipe string, secret []byte, err error) {
	for i, chain := range [][2]string{
		{"XNC_CORE_PIPE", "XNC_CORE_SECRET_HEX"},
		{"XNC_DESKTOP_CORE_PIPE", "XNC_DESKTOP_CORE_SECRET_HEX"},
	} {
		envPipe := os.Getenv(chain[0])
		envHex := os.Getenv(chain[1])
		switch {
		case envPipe != "" && envHex != "":
			secret, err := decodeHexSecret(envHex, chain[1])
			if err != nil {
				return "", nil, err
			}
			return envPipe, secret, nil
		case envPipe != "" || envHex != "":
			if i == 0 {
				continue // legacy 链半缺:试 canonical 链
			}
			return "", nil, errors.New("coreclient: XNC_DESKTOP_CORE_* partially set (need both or neither)")
		}
	}
	if stateDir == "" {
		return "", nil, errors.New("coreclient: no core credentials in env (state-dir fallback needs a state dir)")
	}
	b, err := os.ReadFile(CoreSecretPath(stateDir))
	if err != nil {
		return "", nil, fmt.Errorf("coreclient: no core credentials (env unset, %s unreadable): %w",
			CoreSecretPath(stateDir), err)
	}
	secret, err = decodeHexSecret(strings.TrimSpace(string(b)), DefaultCoreSecretName)
	if err != nil {
		return "", nil, err
	}
	return DefaultCorePipe, secret, nil
}

func decodeHexSecret(hexStr, src string) ([]byte, error) {
	out, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("coreclient: bad %s: %w", src, err)
	}
	return out, nil
}
