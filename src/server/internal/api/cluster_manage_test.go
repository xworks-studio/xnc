package api

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// TestClusterGetAndRename：GET 详情（成员 200/非成员 404）与 PATCH 改名
// （owner 200、重名 400、operator 403）。
func TestClusterGetAndRename(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	cid := defaultClusterID(t, srv.URL, admin)

	opID, opTok := createUserViaAPI(t, srv.URL, admin, "getren-op@t.local", "Op")
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+cid+"/members", admin,
		`{"user_id":"`+opID+`","role":"operator"}`).StatusCode)

	// 成员 → 200，含 personal 与计数。
	resp := doJSON(t, srv.URL, "GET", "/api/clusters/"+cid, admin, "")
	require.Equal(t, 200, resp.StatusCode)
	var detail map[string]any
	require.NoError(t, decodeJSON(resp.Body, &detail))
	assert.Equal(t, "default", detail["name"])
	assert.Equal(t, true, detail["personal"])
	assert.Equal(t, float64(2), detail["memberCount"])

	// 非成员 → 404（不泄漏存在性）。
	_, outTok := createUserViaAPI(t, srv.URL, admin, "getren-out@t.local", "Out")
	assert.Equal(t, 404, doJSON(t, srv.URL, "GET", "/api/clusters/"+cid, outTok, "").StatusCode)

	// operator（非 owner）改名 → 403。
	assert.Equal(t, 403, doJSON(t, srv.URL, "PATCH", "/api/clusters/"+cid, opTok,
		`{"name":"nope"}`).StatusCode)

	// owner 改名 → 200；改名自身同名 → 200（幂等，PG 不报自冲突）；
	// 撞他人 cluster 名 → 400。
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/clusters/"+cid, admin,
		`{"name":"renamed"}`).StatusCode)
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/clusters/"+cid, admin,
		`{"name":"renamed"}`).StatusCode)
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", admin,
		`{"name":"occupied"}`).StatusCode)
	assert.Equal(t, 400, doJSON(t, srv.URL, "PATCH", "/api/clusters/"+cid, admin,
		`{"name":"occupied"}`).StatusCode)
}

// emailToID 经 DB 查用户 id（成员操作断言用）。
func emailToID(t *testing.T, env *TestEnv, email string) string {
	t.Helper()
	var id string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT id::text FROM users WHERE email=$1`, email).Scan(&id))
	return id
}

// TestMemberEmailAndRole：按 email 加成员（owner 无须 admin 列全量用户）、
// 404 USER_NOT_FOUND、改角色提升/降级、最后 owner 保护、非成员 404。
func TestMemberEmailAndRole(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	cid := defaultClusterID(t, srv.URL, admin)

	_, tok := createUserViaAPI(t, srv.URL, admin, "alice@t.local", "Alice")
	// 按 email 添加为 operator → 201。
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+cid+"/members", admin,
		`{"email":"alice@t.local","role":"operator"}`).StatusCode)
	// 未注册 email → 404 USER_NOT_FOUND。
	resp := doJSON(t, srv.URL, "POST", "/api/clusters/"+cid+"/members", admin,
		`{"email":"ghost@t.local","role":"viewer"}`)
	require.Equal(t, 404, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &e))
	assert.Equal(t, proto.CodeUserNotFound, e.Error.Code)
	// 非 owner 加成员 → 403。
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/clusters/"+cid+"/members", tok,
		`{"email":"bob@t.local","role":"viewer"}`).StatusCode)

	// admin 是 default cluster 唯一 owner：降级 admin 自己 → 400 最后 owner。
	adminID := env.AdminUUID(t).String()
	assert.Equal(t, 400, doJSON(t, srv.URL, "PATCH",
		"/api/clusters/"+cid+"/members/"+adminID, admin,
		`{"role":"viewer"}`).StatusCode)
	// 提升 alice 为 owner → 200；此时降级 admin（仍有另一个 owner）→ 200。
	aliceID := emailToID(t, env, "alice@t.local")
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH",
		"/api/clusters/"+cid+"/members/"+aliceID, admin, `{"role":"owner"}`).StatusCode)
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH",
		"/api/clusters/"+cid+"/members/"+adminID, admin, `{"role":"viewer"}`).StatusCode)

	// alice 成唯一 owner：她移除/降级自己 → 400；非成员改角色 → 404；
	// admin（已 viewer）再操作成员 → 403。
	assert.Equal(t, 400, doJSON(t, srv.URL, "DELETE",
		"/api/clusters/"+cid+"/members/"+aliceID, tok, "").StatusCode)
	assert.Equal(t, 400, doJSON(t, srv.URL, "PATCH",
		"/api/clusters/"+cid+"/members/"+aliceID, tok, `{"role":"operator"}`).StatusCode)
	assert.Equal(t, 404, doJSON(t, srv.URL, "PATCH",
		"/api/clusters/"+cid+"/members/00000000-0000-0000-0000-000000000000", tok,
		`{"role":"viewer"}`).StatusCode)
	assert.Equal(t, 403, doJSON(t, srv.URL, "DELETE",
		"/api/clusters/"+cid+"/members/"+aliceID, admin, "").StatusCode)
}

// TestConcurrentOwnerDemote：恰好两个 owner 并发互相降级——membership 锁
// 串行化后恰一个成功，cluster 永远保有 owner。
func TestConcurrentOwnerDemote(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	aID, aTok := createUserViaAPI(t, srv.URL, admin, "race-a@t.local", "A")
	bID, bTok := createUserViaAPI(t, srv.URL, admin, "race-b@t.local", "B")
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", aTok,
		`{"name":"race"}`).StatusCode)
	var cl []map[string]any
	resp := doJSON(t, srv.URL, "GET", "/api/clusters", aTok, "")
	require.NoError(t, decodeJSON(resp.Body, &cl))
	var cid string
	for _, c := range cl {
		if c["name"] == "race" {
			cid = c["id"].(string)
		}
	}
	require.NotEmpty(t, cid)
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+cid+"/members", aTok,
		`{"user_id":"`+bID+`","role":"owner"}`).StatusCode)

	// A 降 B 与 B 降 A 并发：锁串行化后恰一个成功；另一个被拦（若发起者已被
	// 降级则 authorizeClusterOwner 报 403，否则最后 owner 守卫报 400）。
	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, call := range []struct {
		tok, target string
	}{{aTok, bID}, {bTok, aID}} {
		wg.Add(1)
		go func(tok, target string) {
			defer wg.Done()
			<-start
			resp := doJSON(t, srv.URL, "PATCH",
				"/api/clusters/"+cid+"/members/"+target, tok, `{"role":"viewer"}`)
			statuses <- resp.StatusCode
		}(call.tok, call.target)
	}
	close(start)
	wg.Wait()
	close(statuses)
	var ok, blocked int
	for s := range statuses {
		switch s {
		case 200:
			ok++
		case 400, 403:
			blocked++
		}
	}
	assert.Equal(t, 1, ok, "exactly one demote succeeds")
	assert.Equal(t, 1, blocked, "the other is blocked (last-owner guard or lost owner role)")

	// 终态：仍有一个 owner。
	var owners int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM cluster_members WHERE cluster_id=$1 AND role='owner'`, cid).Scan(&owners))
	assert.Equal(t, 1, owners)
}

// TestUpdateUserIsAdmin：admin 授/撤 is_admin、display_name、最后 admin 保护、
// 非 admin 403。
func TestUpdateUserIsAdmin(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	uid, tok := createUserViaAPI(t, srv.URL, admin, "grant@t.local", "Grant")

	// 非 admin PATCH → 403；admin 改 display_name → 200。
	assert.Equal(t, 403, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, tok,
		`{"display_name":"X"}`).StatusCode)
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, admin,
		`{"display_name":"New Name"}`).StatusCode)

	// 授 is_admin → 生效（listUsers 含 is_admin；能访问 /api/audit）。
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, admin,
		`{"is_admin":true}`).StatusCode)
	assert.Equal(t, 200, doJSON(t, srv.URL, "GET", "/api/audit", tok, "").StatusCode)

	// 撤 → 再撤（此时只剩 admin 一个 is_admin）→ 400 LAST_ADMIN。
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, admin,
		`{"is_admin":false}`).StatusCode)
	resp := doJSON(t, srv.URL, "PATCH", "/api/users/"+env.AdminUUID(t).String(), admin,
		`{"is_admin":false}`)
	require.Equal(t, 400, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &e))
	assert.Equal(t, proto.CodeLastAdmin, e.Error.Code)
}

// TestDeleteClusterNameReuse：软删除打 deleted_at（不再改名），同名可重建，
// 已删 cluster 从列表消失。
func TestDeleteClusterNameReuse(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", admin,
		`{"name":"tmp"}`).StatusCode)
	var cl []map[string]any
	require.NoError(t, decodeJSON(doJSON(t, srv.URL, "GET", "/api/clusters", admin, "").Body, &cl))
	var cid string
	for _, c := range cl {
		if c["name"] == "tmp" {
			cid = c["id"].(string)
		}
	}
	require.NotEmpty(t, cid)
	assert.Equal(t, 204, doJSON(t, srv.URL, "DELETE", "/api/clusters/"+cid, admin, "").StatusCode)

	// 列表不含已删（改名形态也不得残留）；原名可重建；原名被占后他人撞名 400。
	require.NoError(t, decodeJSON(doJSON(t, srv.URL, "GET", "/api/clusters", admin, "").Body, &cl))
	for _, c := range cl {
		assert.NotEqual(t, "tmp", c["name"], "deleted cluster must not be listed")
		assert.NotContains(t, c["name"], "tmp_deleted_",
			"deletion must tombstone via deleted_at, not rename")
	}
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", admin,
		`{"name":"tmp"}`).StatusCode)
	_, other := createUserViaAPI(t, srv.URL, admin, "other@t.local", "Other")
	assert.Equal(t, 400, doJSON(t, srv.URL, "POST", "/api/clusters", other,
		`{"name":"tmp"}`).StatusCode)
}
