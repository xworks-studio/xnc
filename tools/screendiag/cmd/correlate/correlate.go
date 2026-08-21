// correlate — 判定解码视频帧相对 GDI 快照参考的几何变换。
// 将两图降采样为网格后计算 4 种变换的归一化相关系数：
//
//	go run correlate.go <frame.png> <snap.jpg>
//
// 输出 identity / flipX / flipY / rot180 / zoom2x(identity, 左上 1/4 缩放)
// 各自的相关分（1.0 = 完全一致），最高分即判定结果。
package main

import (
	"fmt"
	"image/color"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
)

func main() {
	frame := loadGray(os.Args[1])
	snap := loadGray(os.Args[2])

	const gw, gh = 160, 100
	f := grid(frame, gw, gh)
	s := grid(snap, gw, gh)

	flippedX := flipX(f)
	fmt.Printf("identity  corr=%+.3f\n", corr(f, s))
	fmt.Printf("flipX     corr=%+.3f\n", corr(flippedX, s))
	fmt.Printf("flipY     corr=%+.3f\n", corr(flipY(f), s))
	fmt.Printf("rot180    corr=%+.3f\n", corr(flipX(flipY(f)), s))
	// zoom2x 假设：帧是桌面的 2 倍放大（左上 1/4 区域）
	quad := crop(f, gw/2, gh/2)
	fmt.Printf("zoom2x(TL→full downscaled) corr=%+.3f\n", corr(quad, s))
}

func loadGray(path string) *image.Gray {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		panic(err)
	}
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, gr, bl, _ := img.At(x, y).RGBA()
			v := uint8((r + gr + bl) / 768); g.SetGray(x-b.Min.X, y-b.Min.Y, color.Gray{v})
		}
	}
	return g
}

// grid 平均池化到 gw×gh。
func grid(g *image.Gray, gw, gh int) []float64 {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	out := make([]float64, gw*gh)
	for gy := 0; gy < gh; gy++ {
		for gx := 0; gx < gw; gx++ {
			var sum float64
			var n int
			for y := gy * h / gh; y < (gy+1)*h/gh; y++ {
				for x := gx * w / gw; x < (gx+1)*w/gw; x++ {
					sum += float64(g.GrayAt(x, y).Y)
					n++
				}
			}
			out[gy*gw+gx] = sum / float64(n)
		}
	}
	return out
}

const gw, gh = 160, 100

func flipX(g []float64) []float64 {
	out := make([]float64, len(g))
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			out[y*gw+x] = g[y*gw+(gw-1-x)]
		}
	}
	return out
}

func flipY(g []float64) []float64 {
	out := make([]float64, len(g))
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			out[y*gw+x] = g[(gh-1-y)*gw+x]
		}
	}
	return out
}

func crop(g []float64, w, h int) []float64 {
	out := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out[y*w+x] = g[y*160+x]
		}
	}
	return out
}

func corr(a, b []float64) float64 {
	ma, mb := mean(a), mean(b)
	var num, da, db float64
	for i := range a {
		num += (a[i] - ma) * (b[i] - mb)
		da += (a[i] - ma) * (a[i] - ma)
		db += (b[i] - mb) * (b[i] - mb)
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / (sqrt(da) * sqrt(db))
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	r := x
	for i := 0; i < 40; i++ {
		r = (r + x/r) / 2
	}
	return r
}
