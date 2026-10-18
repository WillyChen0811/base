package wgtund

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
)

// 整体启动：只有 target 的转发不开本机口，/forwards 带 target 和「其他（直连）」，启动信息与 /status 带 proxy
func TestProxyRunEndToEnd(t *testing.T) {
	real := startFwdUpstream(t, "real")
	f := newFixture(t, func(cfg map[string]any) {
		cfg["proxy"] = "127.0.0.1:0"
		addFwd(cfg, map[string]any{"name": "smb", "target": []string{"10.116.7.250:445", "10.9.0.0/16"},
			"routes": []map[string]any{fwdRoute("A", "", "127.0.0.1:1")}})
	})
	st, _, _ := run(t, f.dir)
	if st.Proxy == nil || !strings.HasPrefix(st.Proxy.Addr, "127.0.0.1:") || st.Proxy.Error != "" {
		t.Fatalf("启动信息 proxy = %+v", st.Proxy)
	}
	host, port, _ := net.SplitHostPort(real.addr())
	pn, _ := strconv.Atoi(port)
	c := connectOK(t, st.Proxy.Addr, host, pn)
	if l := askOnce(t, c, "hey"); l != "real:hey\n" {
		t.Errorf("回程 = %q", l)
	}
	_, b := get(t, st, "/forwards", true)
	var fs []forwardView
	if err := json.Unmarshal(b, &fs); err != nil || len(fs) != 2 {
		t.Fatalf("/forwards = %s", b)
	}
	if fs[0].Listen != "" || len(fs[0].Target) != 2 || fs[1].Name != directName {
		t.Errorf("/forwards = %+v", fs)
	}
	if !strings.Contains(string(b), `"up_total":4`) { // "hey\n"
		t.Errorf("直连流量没计上: %s", b)
	}
	_, sb := get(t, st, "/status", true)
	if !strings.Contains(string(sb), `"proxy":{"addr":"`+st.Proxy.Addr) {
		t.Errorf("/status 应带 proxy: %s", sb)
	}
}

func TestProxyBadConfig(t *testing.T) {
	a := "127.0.0.1:1"
	tf := func(name string, target ...string) map[string]any {
		return map[string]any{"name": name, "target": target, "routes": []map[string]any{fwdRoute("", "", a)}}
	}
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"target 重复": {func(c map[string]any) {
			addFwd(c, tf("a", "10.116.7.250:445"), tf("b", "10.116.7.250:445"))
		}, "重复"},
		"target 大小写不同也算重复": {func(c map[string]any) {
			addFwd(c, tf("a", "Foo.example"), tf("b", "foo.example"))
		}, "重复"},
		"target 端口非数字":        {func(c map[string]any) { addFwd(c, tf("a", "10.1.1.1:smb")) }, "端口无效"},
		"target CIDR 非法":      {func(c map[string]any) { addFwd(c, tf("a", "10.1.1.1/40")) }, "CIDR"},
		"target 是 IPv6":       {func(c map[string]any) { addFwd(c, tf("a", "::1")) }, "IPv6"},
		"listen 与 target 都没写": {func(c map[string]any) { addFwd(c, tf("a")) }, "至少写一个"},
		"proxy 非回环":           {func(c map[string]any) { c["proxy"] = "0.0.0.0:1080" }, "回环"},
		"proxy 没端口":           {func(c map[string]any) { c["proxy"] = "127.0.0.1" }, "主机:端口"},
		"proxy_default 不存在": {func(c map[string]any) {
			c["proxy"] = "127.0.0.1:0"
			c["proxy_default"] = "nope"
		}, "proxy_default"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, tc.mutate)
			var out strings.Builder
			err := Run(context.Background(), f.dir, strings.NewReader(""), &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报含 %q 的错，得到 %v", tc.want, err)
			}
		})
	}
}

// 没配 proxy 时启动信息不带 proxy 字段，/forwards 里也没有直连项
func TestProxyNotConfigured(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	if st.Proxy != nil {
		t.Errorf("没配 proxy 不应有该字段: %+v", st.Proxy)
	}
	if fs := forwardsOf(t, st); len(fs) != 0 {
		t.Errorf("/forwards = %+v", fs)
	}
}

