// cadprobe - dev-only: attach a desktop rt pipe and report AU arrival
// cadence (fps + interval percentiles) over a fixed window.
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"xnc/agent/desktoppipe"
)

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func main() {
	pipe := os.Args[1]
	secretHex := os.Args[2]
	window := 12 * time.Second
	if len(os.Args) > 3 {
		var s int
		fmt.Sscanf(os.Args[3], "%d", &s)
		window = time.Duration(s) * time.Second
	}
	if !strings.Contains(pipe, `\`) {
		pipe = `\\.\pipe\` + pipe
	}
	sec, _ := hex.DecodeString(secretHex)
	sub, err := desktoppipe.Dial(pipe, string(sec), 0xE2E1, desktoppipe.SubOpts{})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	h := sub.Hello()
	fmt.Printf("hello gen=%d %dx%d@%dfps window=%s\n", h.Gen, h.W, h.H, h.Fps, window)
	deadline := time.Now().Add(window)
	var intervals []float64
	n, keys := 0, 0
	var last time.Time
	firstAt, lastAt := time.Time{}, time.Time{}
	for time.Now().Before(deadline) {
		select {
		case f := <-sub.FrameCh():
			now := time.Now()
			if firstAt.IsZero() {
				firstAt = now
			}
			lastAt = now
			if !last.IsZero() {
				intervals = append(intervals, now.Sub(last).Seconds()*1000)
			}
			last = now
			n++
			if f.Key {
				keys++
			}
		case <-time.After(2 * time.Second):
			fmt.Println("starved: no AU for 2s")
		}
	}
	sorted := append([]float64(nil), intervals...)
	sort.Float64s(sorted)
	el := lastAt.Sub(firstAt).Seconds()
	fmt.Printf("aus=%d keys=%d span=%.2fs fps=%.2f intervals: p50=%.1fms p90=%.1fms p99=%.1fms max=%.1fms\n",
		n, keys, el, float64(n-1)/el, pct(sorted, 0.5), pct(sorted, 0.9), pct(sorted, 0.99), pct(sorted, 1.0))
	_ = sub.Close()
}
