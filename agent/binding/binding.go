// Package binding 管理机器级 server/cluster 绑定（StateDir/binding.json，
// 设计 §5.2）：公开字段、无密钥；写路径原子（同目录 tmp + rename），进程
// 中断最坏残留一个 tmp 文件，绝不产生半写的 binding.json。文件权限/
// DACL 与 identity.json 同一约定（继承 StateDir，0600，不另行手写 ACL）。
package binding

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// fileName 状态目录内固定文件名（CLI/安装器/控制管道按此读写）。
const fileName = "binding.json"

// Binding 是本机到 server/cluster 的注册绑定（spec §5.2 字段，小驼峰）。
// 全部公开字段，无敏感信息。
type Binding struct {
	Server       string    `json:"server"`       // 控制面基址，如 https://xnc.example
	ClusterID    string    `json:"clusterId"`    // 所属 cluster
	NodeID       string    `json:"nodeId"`       // 服务端分配的节点 ID
	RegisteredAt time.Time `json:"registeredAt"` // 绑定写入时刻（遗留合成为近似值）
	Channel      string    `json:"channel"`      // 更新频道（stable/dev；未定则为空）
}

// Path 返回 dir 下 binding.json 的完整路径。
func Path(dir string) string { return filepath.Join(dir, fileName) }

// Load 读取绑定。文件不存在返回 (nil, false, nil)；存在但损坏返回错误
// （是否忽略由调用方决定，不静默当作未绑定）。
func Load(dir string) (*Binding, bool, error) {
	b, err := os.ReadFile(Path(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v Binding
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, false, err
	}
	return &v, true, nil
}

// Save 原子写入绑定：先写同目录临时文件再 rename 顶替（Windows 上
// rename 即 MoveFileEx REPLACE_EXISTING，覆盖写成立）。目录不存在则创建
// （0700，与 identity.Save 同一权限约定）。
func Save(dir string, v *Binding) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// 失败路径清理 tmp：留下残档无害，但清干净更好。
	defer func() {
		if err != nil {
			tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(b); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	err = os.Rename(name, Path(dir))
	return err
}
