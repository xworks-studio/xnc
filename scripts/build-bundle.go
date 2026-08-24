// build-bundle.go — 打包自更新 bundle（manifest + agent + helper → tar.gz）。
// 用法: go run scripts/build-bundle.go <binDir> <version> <out.tar.gz>
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type manifest struct {
	Version string `json:"version"`
	Files   []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func main() {
	dir, version, out := os.Args[1], os.Args[2], os.Args[3]
	names := []string{
		"xnc-agent.exe",
		"xnc-core.exe",        // prod bootstrap: XNCCore service binary
		"xnc-desktop.exe",     // capture host (spawned by core)
		"xnc-shell.exe",       // ConPTY/oneshot host (spawned by core)
		"xnc-screen-helper.exe", // updater manifest still requires it
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
