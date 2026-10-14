package wgtund

import (
	"strings"
	"testing"
)

// 通用审批走外层路由：decide 只许从 xxyi 页面发起（无/坏 Origin 403，白名单 Origin 进业务 404）；
// GET /approval 转发到 support 列待批；没配协助一律 404。
func TestApprovalRoutesWired(t *testing.T) {
	f, _, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	for _, p := range []string{"/approval/decide?id=x&allow=1", "/approval/decide?id=x&allow=0&message=no"} {
		if resp, b := supportPost(t, st, p, ""); resp.StatusCode != 403 || !strings.Contains(string(b), "需要从 xxyi 页面操作") {
			t.Errorf("无 Origin 的 POST %s 应 403: %d %s", p, resp.StatusCode, b)
		}
		if resp, _ := supportPost(t, st, p, "http://evil.example"); resp.StatusCode != 403 {
			t.Errorf("不在白名单的 Origin POST %s 应 403，得到 %d", p, resp.StatusCode)
		}
		// 带白名单 Origin 进业务逻辑：查无此条 404（不是 403）
		if resp, _ := supportPost(t, st, p, testOrigin); resp.StatusCode != 404 {
			t.Errorf("带 Origin 的 POST %s 应进业务得 404，得到 %d", p, resp.StatusCode)
		}
	}
	// GET /approval 转发到 support：待批列表（不要 Origin）
	if resp, b := get(t, st, "/approval", true); resp.StatusCode != 200 || !strings.Contains(string(b), "requests") {
		t.Errorf("GET /approval 应 200 带 requests: %d %s", resp.StatusCode, b)
	}
	// 没配协助：/approval 和 /approval/decide（带 Origin）都 404 说清
	f2 := newFixture(t, nil)
	st2, _, _ := run(t, f2.dir)
	if resp, b := get(t, st2, "/approval", true); resp.StatusCode != 404 || !strings.Contains(string(b), "没有配置协助") {
		t.Errorf("没配协助 GET /approval 应 404 没有配置协助: %d %s", resp.StatusCode, b)
	}
	if resp, b := supportPost(t, st2, "/approval/decide?id=x&allow=1", testOrigin); resp.StatusCode != 404 || !strings.Contains(string(b), "没有配置协助") {
		t.Errorf("没配协助 POST /approval/decide 应 404 没有配置协助: %d %s", resp.StatusCode, b)
	}
}
