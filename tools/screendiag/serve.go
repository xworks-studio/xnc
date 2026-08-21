//go:build ignore

// serve.go — 一次性静态文件服务器（诊断用）：go run serve.go <dir> <port>
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run serve.go <dir> <port>")
		os.Exit(2)
	}
	fmt.Println("serving " + os.Args[1] + " on http://127.0.0.1:" + os.Args[2])
	panic(http.ListenAndServe("127.0.0.1:"+os.Args[2], http.FileServer(http.Dir(os.Args[1]))))
}
