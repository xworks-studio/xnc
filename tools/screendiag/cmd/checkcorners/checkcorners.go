// checkcorners — 校验 ffmpeg rgb24 裸流的四象限颜色（编码正确性判定）。
// 用法: go run checkcorners.go <file.raw> <width> <height>
package main

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	w, _ := strconv.Atoi(os.Args[2])
	h, _ := strconv.Atoi(os.Args[3])
	px := func(x, y int) (int, int, int) {
		i := (y*w + x) * 3
		return int(b[i]), int(b[i+1]), int(b[i+2])
	}
	quad := func(qx, qy int) (int, int, int) {
		cx, cy := 0, 0
		if qx == 1 {
			cx = w / 2
		}
		if qy == 1 {
			cy = h / 2
		}
		var rs, gs, bs, n int
		for y := cy + h/10; y < cy+h/10+h/20; y += 7 {
			for x := cx + w/10; x < cx+w/10+w/20; x += 7 {
				r, g, bl := px(x, y)
				rs, gs, bs, n = rs+r, gs+g, bs+bl, n+1
			}
		}
		return rs / n, gs / n, bs / n
	}
	var q [4][3]int
	for i, qq := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		q[i][0], q[i][1], q[i][2] = quad(qq[0], qq[1])
	}
	names := [4]string{"top-left   ", "top-right  ", "bottom-left", "bottom-right"}
	want := [4][3]int{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {255, 255, 255}}
	wnames := [4]string{"RED", "GREEN", "BLUE", "WHITE"}
	allOK := true
	for i := 0; i < 4; i++ {
		ok := abs(q[i][0]-want[i][0]) < 60 && abs(q[i][1]-want[i][1]) < 60 && abs(q[i][2]-want[i][2]) < 60
		if !ok {
			allOK = false
		}
		fmt.Printf("%s RGB(%3d,%3d,%3d) expect %-5s → %s\n",
			names[i], q[i][0], q[i][1], q[i][2], wnames[i], map[bool]string{true: "OK", false: "WRONG"}[ok])
	}
	if !allOK {
		os.Exit(1)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
