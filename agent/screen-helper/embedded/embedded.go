// Package embedded 携带 xnc-dda.dll（DXGI 采集层，C 实现）。
//
// 运行期加载顺序（ddaloader_windows.go）：exe 同目录 → 不存在则把嵌入
// 副本解压到 exe 同目录再加载。真实 DLL 由 dda/build.bat 产出并拷入本
// 目录；本目录只跟踪占位 README（DLL 已 gitignore），未跑 build.bat 时
// Go 构建仍通过（目录里只有 README），运行期给出明确报错提示。
package embedded

import "embed"

//go:embed *
var files embed.FS

// DLLBytes 返回嵌入的 xnc-dda.dll；未构建（目录里只有占位文件）时返回
// fs.ErrNotExist，上层转为"先运行 dda/build.bat"提示。
func DLLBytes() ([]byte, error) {
	return files.ReadFile("xnc-dda.dll")
}
