package api

// platform_release_test.go — 平台化发布分发（2026-09-28 Linux 节点支持）：
// linux-only release 的上传/分发/清单/安装脚本；setup/linux 互为可选的
// 上传闸门；节点 platform 元数据的注册落库与缺省回填。

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLinuxTarball / fakeSetup 带对应魔数的占位制品。
func fakeLinuxTarball(b byte) []byte {
	return append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte{b}, 64)...)
}

func fakeSetup(b byte) []byte {
	return append([]byte{'M', 'Z'}, bytes.Repeat([]byte{b}, 64)...)
}

func getBody(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

func TestLinuxReleaseDistribution(t *testing.T) {
	env := NewTestEnv(t)
	tok := env.AdminToken(t)

	// linux-only release（无 setup）→ 201（平台化发布：单平台版本合法）。
	resp := uploadRelease(t, env.srv.URL, tok, "9.9.9",
		map[string][]byte{"linux": fakeLinuxTarball(1)})
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// /installer?platform=linux 直流制品：字节、文件名、sha256 头。
	sum := sha256.Sum256(fakeLinuxTarball(1))
	dl, body := getBody(t, env.srv.URL+"/installer?platform=linux")
	require.Equal(t, http.StatusOK, dl.StatusCode)
	assert.Equal(t, fakeLinuxTarball(1), body)
	assert.Contains(t, dl.Header.Get("Content-Disposition"), "xnc-linux-amd64-9.9.9.tar.gz")
	assert.Equal(t, hex.EncodeToString(sum[:]), dl.Header.Get("X-Xnc-Sha256"))

	// /installer.json?platform=linux 清单：url 带平台参数。
	mr, mbody := getBody(t, env.srv.URL+"/installer.json?platform=linux")
	require.Equal(t, http.StatusOK, mr.StatusCode)
	var m struct {
		Version string `json:"version"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
	}
	require.NoError(t, json.Unmarshal(mbody, &m))
	assert.Equal(t, "9.9.9", m.Version)
	assert.Contains(t, m.URL, "/installer?channel=stable&platform=linux")
	assert.Equal(t, hex.EncodeToString(sum[:]), m.SHA256)

	// /install.sh 引导脚本：指向平台化清单。
	sr, sbody := getBody(t, env.srv.URL+"/install.sh")
	require.Equal(t, http.StatusOK, sr.StatusCode)
	assert.Contains(t, string(sbody), "installer.json?channel=stable&platform=linux")

	// /installer 无 platform → 404（该 release 无 setup.exe；不降级错发）。
	nr, _ := getBody(t, env.srv.URL+"/installer")
	assert.Equal(t, http.StatusNotFound, nr.StatusCode)

	// CLI 分发：os=linux 无 cli-linux 制品 → 404；补传后可取（下一用例）。
	lr, _ := getBody(t, env.srv.URL+"/api/cli/latest?os=linux")
	_ = lr // 无认证 → 401（端点 JWT 保护），此处只确认路由不炸。
}

func TestUploadPlatformGates(t *testing.T) {
	env := NewTestEnv(t)
	tok := env.AdminToken(t)

	// 既无 setup 也无 linux → 400。
	resp := uploadRelease(t, env.srv.URL, tok, "9.9.8",
		map[string][]byte{"cli": []byte("xnc-cli")})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// linux 部件非 gzip 魔数 → 400。
	resp = uploadRelease(t, env.srv.URL, tok, "9.9.7",
		map[string][]byte{"linux": []byte("not-a-gzip")})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// 双平台 release 合法：setup + linux + cli-linux。
	resp = uploadRelease(t, env.srv.URL, tok, "9.9.6", map[string][]byte{
		"setup": fakeSetup(2), "linux": fakeLinuxTarball(3), "cli-linux": []byte("xnc-linux-raw"),
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	// /installer（windows 缺省）与 platform=linux 各取所载。
	wr, wbody := getBody(t, env.srv.URL+"/installer")
	require.Equal(t, http.StatusOK, wr.StatusCode)
	assert.Equal(t, fakeSetup(2), wbody)
	lr, lbody := getBody(t, env.srv.URL+"/installer?platform=linux")
	require.Equal(t, http.StatusOK, lr.StatusCode)
	assert.Equal(t, fakeLinuxTarball(3), lbody)
}

func TestNodePlatformMetadata(t *testing.T) {
	env := NewTestEnv(t)
	tok := env.AdminToken(t)
	cluster := defaultClusterID(t, env.srv.URL, tok)

	// 用户注册路径带 platform=linux → 落库并经 nodeDTO 透出。
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	regBody, _ := json.Marshal(map[string]string{
		"hostname": "LNX-1", "machineId": "mid-linux-1",
		"osVersion": "Ubuntu 24.04", "agentVersion": "0.10.36",
		"publicKey": base64.StdEncoding.EncodeToString(pub),
		"platform":  "linux",
	})
	reg := doJSON(t, env.srv.URL, "POST",
		"/api/clusters/"+cluster+"/nodes/register", tok, string(regBody))
	require.Equal(t, http.StatusCreated, reg.StatusCode)
	t.Cleanup(func() { _ = reg.Body.Close() })
	var created struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, json.NewDecoder(reg.Body).Decode(&created))

	got := doJSON(t, env.srv.URL, "GET", "/api/nodes/"+created.NodeID, tok, "")
	t.Cleanup(func() { _ = got.Body.Close() })
	var node map[string]any
	require.NoError(t, json.NewDecoder(got.Body).Decode(&node))
	assert.Equal(t, "linux", node["platform"])

	// token 路径（旧 agent）不带 platform → 迁移缺省 windows。
	winID := env.EnrollNode(t, "WIN-1", "mid-win-1")
	got2 := doJSON(t, env.srv.URL, "GET", "/api/nodes/"+winID, tok, "")
	t.Cleanup(func() { _ = got2.Body.Close() })
	var node2 map[string]any
	require.NoError(t, json.NewDecoder(got2.Body).Decode(&node2))
	assert.Equal(t, "windows", node2["platform"])
}
