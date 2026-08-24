// build-bundle.go — 打包自更新 bundle（manifest + agent + core/desktop/shell → tar.gz）。
// 用法: go run scripts/build-bundle.go [--include-helper] <binDir> <version> <out.tar.gz>
//
// --include-helper: 额外打包 xnc-screen-helper.exe（从 <binDir> 读）。仅用于
// 0.4.6 过渡 bundle——0.4.5 agent 的 requiredFiles 仍含已退役的 helper，
// 缺它 stage 校验会拒收 0.4.6 bundle；0.4.6+ agent 的 requiredFiles 不含
// helper，apply 只搬 4 个必需文件，helper 留在 staging（apply 成功/回滚都
// 会清掉 staging，不落盘——见 agent/updater/apply_windows.go RunApply）。
//
// 版本单一来源：manifest 版本必须与 agent 自报版本一致（agent 构建时经
// -ldflags 注入 xnc/agent/machineinfo.Version；未注入回落 0.0.0-dev）。
// 例:
//
//	cd agent && go build -ldflags "-X xnc/agent/machineinfo.Version=0.4.6" -o ../bin/xnc-agent.exe ./cmd/xnc-agent
//	go run scripts/build-bundle.go bin 0.4.6 bin/bundle-0.4.6.tar.gz
//	go run scripts/build-bundle.go --include-helper bin 0.4.6 bin/bundle-0.4.6-helper.tar.gz
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type manifest struct {
	Version string `json:"version"`
	Files   []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func main() {
	includeHelper := flag.Bool("include-helper", false,
		"also package xnc-screen-helper.exe (0.4.6 transition bundles only)")
	flag.Parse()
	args := flag.Args()
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/build-bundle.go [--include-helper] <binDir> <version> <out.tar.gz>")
		os.Exit(2)
	}
	dir, version, out := args[0], args[1], args[2]
	// 版本单一来源校验：manifest 版本必须等于 agent 自报版本（agent 构建时
	// 经 -ldflags 注入）。防止「bundle 版本与 agent 二进制版本脱节」复发
	// （0.4.0 生产死循环的根因）。
	agentExe := filepath.Join(dir, "xnc-agent.exe")
	reported := selfReportedVersion(agentExe)
	if reported == "" {
		fmt.Fprintln(os.Stderr, "warning: cannot run "+agentExe+" --version on this host (non-Windows?);")
		fmt.Fprintln(os.Stderr, "  ensure the agent was built with -ldflags \"-X xnc/agent/machineinfo.Version="+version+"\"")
	} else if reported != version {
		fmt.Fprintf(os.Stderr, "ERROR: bundle version %q != agent self-reported version %q\n", version, reported)
		fmt.Fprintln(os.Stderr, "  rebuild the agent with -ldflags \"-X xnc/agent/machineinfo.Version="+version+"\" (see Makefile build-agent)")
		os.Exit(1)
	} else {
		fmt.Println("agent self-reported version:", reported, "(matches bundle version)")
	}
	names := []string{
		"xnc-agent.exe",
		"xnc-core.exe",    // prod bootstrap: XNCCore service binary
		"xnc-desktop.exe", // capture host (spawned by core)
		"xnc-shell.exe",   // ConPTY/oneshot host (spawned by core)
	}
	if *includeHelper {
		// 0.4.6 过渡 bundle：0.4.5 agent 的 requiredFiles 含已退役的
		// xnc-screen-helper.exe，stage 校验缺它会拒收。manifest 照实列出
		// （0.4.6+ agent stage 逐文件校验会一起验）；apply 只搬 requiredFiles
		// 4 个，helper 不落盘（staging 在 apply 成功/回滚后删除）。
		names = append(names, "xnc-screen-helper.exe")
	}
	var mf manifest
	mf.Version = version
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		s := sha256.Sum256(b)
		mf.Files = append(mf.Files, struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
		}{n, hex.EncodeToString(s[:])})
	}
	mj, _ := json.MarshalIndent(mf, "", "  ")
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	hdr := &tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(mj))}
	tw.WriteHeader(hdr)
	tw.Write(mj)
	for _, n := range names {
		b, _ := os.ReadFile(filepath.Join(dir, n))
		hdr := &tar.Header{Name: n, Mode: 0o755, Size: int64(len(b))}
		tw.WriteHeader(hdr)
		tw.Write(b)
	}
	fmt.Println("bundle:", out)
}

// selfReportedVersion 运行已构建的 agent 读取其自报版本（--version 输出）。
// 非 Windows 宿主无法运行 exe 时返回 ""，由调用方降级为提示。
func selfReportedVersion(agentExe string) string {
	out, err := exec.Command(agentExe, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
