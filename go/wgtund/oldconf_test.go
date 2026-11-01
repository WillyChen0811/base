package wgtund

// 协助/消息/审批拆除后，用户机器上的旧 hosts.json 里还留着 support 段：新 wgtund 要无视它，照常启动。

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// 旧版 hosts.json 里完整的 support 段（字段取自拆除前的 supportConf）。
func oldSupportSection(dir string) map[string]any {
	return map[string]any{
		"host":              "ubuntu",
		"claude_bin":        "/usr/local/bin/claude",
		"workdir":           filepath.Join(dir, "该目录并不存在"), // 旧校验会因此报错，新 wgtund 不该再看它
		"claude_settings":   filepath.Join(dir, "settings.json"),
		"claude_args":       []string{"--model", "sonnet"},
		"repos":             map[string]any{"xxyi": map[string]any{"dir": "/nonexistent", "remote": "ssh://x@h/r"}},
		"git_tunnel":        "home",
		"autostart":         true,
		"autostart_minutes": 30,
	}
}

func TestLoadConfIgnoresOldSupportSection(t *testing.T) {
	for name, support := range map[string]any{
		"完整的旧 support 段": oldSupportSection("/x"),
		"null":           nil,
		"类型都不对":          "not-an-object",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(cfg map[string]any) { cfg["support"] = support })
			conf, err := loadConf(f.dir)
			if err != nil {
				t.Fatalf("带 support 段的旧配置应能加载: %v", err)
			}
			if len(conf.Hosts) != 1 || conf.Hosts[0].Name != "ubuntu" {
				t.Fatalf("主机清单没读对: %+v", conf.Hosts)
			}
		})
	}
}

func TestRunWithOldSupportSection(t *testing.T) {
	f := newFixture(t, func(cfg map[string]any) { cfg["support"] = oldSupportSection(t.TempDir()) })
	if _, err := os.Stat(filepath.Join(f.dir, "hosts.json")); err != nil {
		t.Fatal(err)
	}
	st, _, _ := run(t, f.dir) // 旧校验会因 workdir 不存在而让 Run 失败；这里必须正常起来
	if resp, b := get(t, st, "/hosts", true); resp.StatusCode != http.StatusOK {
		t.Fatalf("/hosts 应 200: %d %s", resp.StatusCode, b)
	}
	// 协助、消息、审批的路由都没了：带令牌也是 404，带 Origin 的 POST 也一样。
	for _, p := range []string{"/support", "/support/start", "/support/hook", "/msg", "/msg/decide", "/approval", "/approval/decide"} {
		if resp, b := get(t, st, p, true); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s 应 404: %d %s", p, resp.StatusCode, b)
		}
		if resp, b := postOrigin(t, st, p, testOrigin); resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s 应 404: %d %s", p, resp.StatusCode, b)
		}
	}
}
