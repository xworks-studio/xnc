package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// 嵌入内容在 CI/未构建前端时是仓库占位（index.html 含 <div id="root">），
// 本地同步真实产物后是 Vite 输出——两者都必须满足以下不变量。
func TestSPAHandler(t *testing.T) {
	h := SPAHandler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 根路径 → index.html（HTML shell）
	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	body := readAll(t, resp)
	assert.Contains(t, body, `<div id="root">`)

	// SPA fallback：客户端路由路径 → 同一 index.html
	resp2, err := http.Get(srv.URL + "/nodes")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, body, readAll(t, resp2))

	// /api/* 前缀不兜底（挂到 chi 后由 API 路由优先处理，未知 API 保持 404）
	resp3, err := http.Get(srv.URL + "/api/nope")
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode)
}
