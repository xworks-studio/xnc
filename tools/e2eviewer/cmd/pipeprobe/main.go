// pipeprobe - dev-only: attach a desktop rt pipe and print AU structure
// (key flag + NAL types) to debug the e2eviewer keyframe question.
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"xnc/agent/desktoppipe"
)

func main() {
	pipe := os.Args[1] // full path or bare name
	if !strings.Contains(pipe, `\`) {
		pipe = `\\.\pipe\` + pipe
	}
	secret := os.Args[2] // 64 hex
	n := 20
	if len(os.Args) > 3 {
		fmt.Sscanf(os.Args[3], "%d", &n)
	}
	sec, _ := hex.DecodeString(secret)
	sub, err := desktoppipe.Dial(pipe, string(sec), 0xE2E1, desktoppipe.SubOpts{})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	h := sub.Hello()
	fmt.Printf("hello gen=%d %dx%d@%d\n", h.Gen, h.W, h.H, h.Fps)
	for i := 0; i < n; i++ {
		select {
		case f := <-sub.FrameCh():
			fmt.Printf("au#%d key=%v len=%d first16=%x\n", i, f.Key, len(f.AU), f.AU[:min(16, len(f.AU))])
		case <-time.After(5 * time.Second):
			fmt.Println("timeout waiting au", i)
			os.Exit(0)
		}
	}
	_ = sub.Close()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
