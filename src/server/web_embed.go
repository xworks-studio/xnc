// web_embed.go 把 web/dist（Vite 构建产物）经 go:embed 嵌进二进制，实现
// 单镜像分发控制面 UI。嵌入路径相对本文件：server/web/dist。仓库内提交了
// 占位 index.html + .gitkeep（见 .gitignore 的豁免规则），保证未构建前端时
// go build 仍可通过；真实产物由构建链覆盖——Dockerfile 在 go build 前把
// Node 阶段产物拷入 server/web/dist，本地验证 = npm run build + docker 构建
// 同步步骤。开发态不嵌入：npm run dev（Vite dev server 代理 /api 到 :8080）。

package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:web/dist
var webFS embed.FS

// SPAHandler 返回内嵌 UI 的静态文件 + SPA fallback handler：
//   - 命中 dist 内文件（/assets/* hash 资源、favicon.svg）→ 原样 serve；
//   - 其余非 /api/* 路径 → index.html（react-router 客户端路由刷新可存活）；
//   - /api/* 前缀不在此兜底，保持 API 对未知路径的 404 语义；
//   - 目录请求（/assets/ 等）同样回落 index.html，避免目录列表泄漏。
//
// 导出供 internal/api（chi 路由在 /api/* 之后 Mount 兜底）使用；embed 路径
// 与本文件同目录层级，无法移入 internal/api。
func SPAHandler() http.Handler {
	dist, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		// 嵌入内容编译期定型且路径与指令同源，此错误不可达，仅防御。
		panic(err)
	}
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if p := strings.TrimPrefix(r.URL.Path, "/"); p != "" {
			if fi, serr := fs.Stat(dist, p); serr == nil && !fi.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		index, ierr := fs.ReadFile(dist, "index.html")
		if ierr != nil {
			// dist 只有占位文件且缺 index.html 的病态布局。
			http.Error(w, "web ui not built into binary", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// index.html 引用 hash 资源，自身不可长缓存，否则发版后引用不更新。
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
