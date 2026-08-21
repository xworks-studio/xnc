// pipeclient — 本地连接 xnc-screen-helper named pipe 的冒烟客户端：
// 读取帧协议（[1B type][4B len LE][payload]），打印每帧类型/大小，转储
// 首个关键帧到文件。用法: pipeclient <pipeName> <out.h264> [seconds]
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/Microsoft/go-winio"
)

func main() {
	name, out := os.Args[1], os.Args[2]
	secs := 8
	if len(os.Args) > 3 {
		secs, _ = strconv.Atoi(os.Args[3])
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, `\.\pipe\`+name)
	if err != nil {
		fatal("dial: %v", err)
	}
	defer conn.Close()
	fmt.Println("connected")

	f, err := os.Create(out)
	if err != nil {
		fatal("create: %v", err)
	}
	defer f.Close()

	hdr := make([]byte, 5)
	bins, keys := 0, 0
	for {
		conn.SetReadDeadline(time.Now().Add(time.Duration(secs) * time.Second))
		if _, err := io.ReadFull(conn, hdr); err != nil {
			break
		}
		n := binary.LittleEndian.Uint32(hdr[1:5])
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			break
		}
		switch hdr[0] {
		case 0x03:
			fmt.Printf("STATE %s\n", payload)
		case 0x04:
			fmt.Printf("DIMS  %dx%d\n", int32(binary.LittleEndian.Uint32(payload[0:4])), int32(binary.LittleEndian.Uint32(payload[4:8])))
		case 0x01:
			bins++
			keys++
			fmt.Printf("KEY   %dB head=% x\n", n, payload[:8])
			f.Write(payload)
		case 0x02:
			bins++
			fmt.Printf("DELTA %dB\n", n)
			f.Write(payload)
		default:
			fmt.Printf("?0x%02X %dB\n", hdr[0], n)
		}
	}
	fmt.Printf("\nframes=%d keys=%d → %s\n", bins, keys, out)
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "pipeclient: "+f+"\n", a...)
	os.Exit(1)
}
