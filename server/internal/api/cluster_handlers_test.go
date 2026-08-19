package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClusters(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	tok := env.AdminToken(t) // 见 Step 3 助手

	// 默认 cluster 存在
	req, _ := http.NewRequest("GET", srv.URL+"/api/clusters", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	var list []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &list))
	assert.Len(t, list, 1)
	assert.Equal(t, "default", list[0]["name"])

	// 创建新 cluster
	req2, _ := http.NewRequest("POST", srv.URL+"/api/clusters",
		bytes.NewBufferString(`{"name":"lab"}`))
	req2.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, 201, resp2.StatusCode)

	// 重名 → 400
	req3, _ := http.NewRequest("POST", srv.URL+"/api/clusters",
		bytes.NewBufferString(`{"name":"lab"}`))
	req3.Header.Set("Authorization", "Bearer "+tok)
	resp3, err := http.DefaultClient.Do(req3)
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, 400, resp3.StatusCode)

	// 未认证 → 401
	resp4, err := http.Get(srv.URL + "/api/clusters")
	require.NoError(t, err)
	defer resp4.Body.Close()
	assert.Equal(t, 401, resp4.StatusCode)
}
