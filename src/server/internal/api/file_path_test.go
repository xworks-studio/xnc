package api

import "testing"

// TestAbsPathRe：file 会话路径校验接受 Windows（盘符/UNC）与 POSIX（根
// 路径）两族绝对路径，拒绝相对路径与伪协议（sanity 校验，跨平台节点）。
func TestAbsPathRe(t *testing.T) {
	accept := []string{
		`C:\Users\x\file.txt`,
		`d:\data\log.txt`,
		`\\server\share\a.txt`,
		`/home/ubuntu/a.txt`,
		`/var/lib/xnc/binding.json`,
		`/`,
	}
	reject := []string{
		"",
		"file.txt",
		"relative/path.txt",
		`C:relative`,
		"ssh://host/x",
		`\\`, // UNC 前缀后须有内容
		`C:\`,
	}
	for _, p := range accept {
		if !absPathRe.MatchString(p) {
			t.Errorf("absPathRe should accept %q", p)
		}
	}
	for _, p := range reject {
		if absPathRe.MatchString(p) {
			t.Errorf("absPathRe should reject %q", p)
		}
	}
}
