package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/config"
	"xnc/server/internal/version"
)

func TestHealth(t *testing.T) {
	h := NewRouter(nil, config.Config{}, nil) // Phase1 前期允许 nil store；health 不触库
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// version 字段 = 注入变量（测试构建未注入 → 0.0.0-dev 占位）。
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, version.Version, body["version"])
}
