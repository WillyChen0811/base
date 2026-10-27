package projsync

import (
	"os"
	"path/filepath"
	"testing"
)

// 配置路径清单的读取与形状校验（§14.1/§14.4）。内容全部虚构。

const pfcfgGood = `{"version":1,"generated_at":"2026-10-27T10:00:00+08:00","units":[` +
	`{"key":"260403J1301","code":"260403J1301","serials":[1],"rel":"2026/260403J1301-项目己"},` +
	`{"key":"260403J1301R2","code":"260403J1301","serials":[2,3],"rel":"2026/260403J1301-项目己-副本"}]}`

func pfcfgFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "projfiles-paths.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPFLoadConfiguredPaths(t *testing.T) {
	cp, err := PFLoadConfiguredPaths(pfcfgFile(t, pfcfgGood))
	if err != nil {
		t.Fatal(err)
	}
	if cp.GeneratedAt == "" {
		t.Errorf("generated_at 应透传")
	}
	units := cp.Units("260403J1301")
	if len(units) != 2 {
		t.Fatalf("编号下应有两条配置: %+v", units)
	}
	if units[0].Key != "260403J1301" || units[1].Key != "260403J1301R2" {
		t.Errorf("按 key 排序不对: %+v", units)
	}
	if got := cp.Units("260403J9999"); got != nil {
		t.Errorf("没配置的编号应返回 nil: %+v", got)
	}
	if u, ok := cp.Find("260403J1301", "2026/260403J1301-项目己-副本"); !ok || u.Key != "260403J1301R2" {
		t.Errorf("Find 配不上: %+v %v", u, ok)
	}
	if _, ok := cp.Find("260403J1301", "2026/260403J1301-别的"); ok {
		t.Errorf("rel 不同不该配上")
	}
	if _, ok := (&PFConfiguredPaths{}).Find("x", "y"); ok {
		t.Errorf("空清单 Find 应为 false")
	}
}

func TestPFLoadConfiguredPathsReject(t *testing.T) {
	for name, body := range map[string]string{
		"坏 JSON":      `{`,
		"版本不认识":       `{"version":2,"units":[]}`,
		"缺 units 键之外": `{"version":1,"units":[],"extra":1}`,
		"键不是编号前缀":     `{"version":1,"units":[{"key":"XXYI2","code":"260403J1301","rel":"2026/260403J1301-己"}]}`,
		"键 R 后不是数字":   `{"version":1,"units":[{"key":"260403J1301RB","code":"260403J1301","rel":"2026/260403J1301-己"}]}`,
		"编号不合法":       `{"version":1,"units":[{"key":"没有编号","code":"没有编号","rel":"2026/没有编号"}]}`,
		"rel 逃逸":      `{"version":1,"units":[{"key":"260403J1301","code":"260403J1301","rel":"../逃逸"}]}`,
		"rel 太深":      `{"version":1,"units":[{"key":"260403J1301","code":"260403J1301","rel":"a/b/c/d/e/260403J1301-己"}]}`,
		"同 rel 两条":    `{"version":1,"units":[{"key":"260403J1301","code":"260403J1301","rel":"r"},{"key":"260403J1301R2","code":"260403J1301","rel":"r"}]}`,
	} {
		if _, err := PFLoadConfiguredPaths(pfcfgFile(t, body)); err == nil {
			t.Errorf("%s：应返回 error", name)
		}
	}
	// 文件不存在：也是 error，绝不能当「都没配置」
	if _, err := PFLoadConfiguredPaths(filepath.Join(t.TempDir(), "不存在.json")); err == nil {
		t.Error("清单不在应返回 error，不能当空清单放行")
	}
	// 空路径
	if _, err := PFLoadConfiguredPaths(""); err == nil {
		t.Error("路径为空应返回 error")
	}
}

func TestPFLoadConfiguredPathsEmptyUnits(t *testing.T) {
	// units 为空数组是合法清单（XXYI 里还没有配置路径），读出来谁都不挡。
	cp, err := PFLoadConfiguredPaths(pfcfgFile(t, `{"version":1,"units":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Units("260403J1301")) != 0 {
		t.Errorf("应是空清单: %+v", cp.ByCode)
	}
}

// 评审 22-1：清单的 blocked（编号有配置但不能同步）读得出来；缺省为空；形状不合格就坏。
func TestPFLoadConfiguredPathsBlocked(t *testing.T) {
	body := `{"version":1,"units":[` +
		`{"key":"260403J1301R2","code":"260403J1301","serials":[2],"rel":"2026/260403J1301-项目己-副本"}],` +
		`"blocked":[{"code":"260403J1301","reason":"主目录 /别处/260403J1301-正式 不在工作目录下，不能同步"},` +
		`{"code":"260403J1302"}]}`
	cp, err := PFLoadConfiguredPaths(pfcfgFile(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cp.BlockedReason("260403J1301"); !ok || r != "主目录 /别处/260403J1301-正式 不在工作目录下，不能同步" {
		t.Errorf("应读出 reason: %q %v", r, ok)
	}
	if r, ok := cp.BlockedReason("260403J1302"); !ok || r != "" {
		t.Errorf("reason 可以省略（读出空串但仍是 blocked）: %q %v", r, ok)
	}
	if _, ok := cp.BlockedReason("260403J9999"); ok {
		t.Error("不在 blocked 里的编号不该被挡")
	}
	// blocked 与同编号的另配单元可以并存（另配目录单元照常在 units 里）
	if len(cp.Units("260403J1301")) != 1 {
		t.Errorf("同编号的另配单元应照常读出: %+v", cp.ByCode)
	}
	// 没有 blocked 字段：空，且不影响原有清单
	cp2, err := PFLoadConfiguredPaths(pfcfgFile(t, pfcfgGood))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cp2.BlockedReason("260403J1301"); ok || len(cp2.Blocked) != 0 {
		t.Errorf("没有 blocked 字段应为空: %+v", cp2.Blocked)
	}
	// 空数组也行
	if _, err := PFLoadConfiguredPaths(pfcfgFile(t, `{"version":1,"units":[],"blocked":[]}`)); err != nil {
		t.Errorf("blocked 为空数组应合格: %v", err)
	}
	// nil 清单不挡
	var nilp *PFConfiguredPaths
	if _, ok := nilp.BlockedReason("260403J1301"); ok {
		t.Error("nil 清单不该报 blocked")
	}
}

func TestPFLoadConfiguredPathsBlockedReject(t *testing.T) {
	for name, body := range map[string]string{
		"编号不合法":        `{"version":1,"units":[],"blocked":[{"code":"没有编号","reason":"x"}]}`,
		"编号为空":         `{"version":1,"units":[],"blocked":[{"code":"","reason":"x"}]}`,
		"编号不是归一化形式":    `{"version":1,"units":[],"blocked":[{"code":"260403j1301","reason":"x"}]}`,
		"编号是带 R 的键":    `{"version":1,"units":[],"blocked":[{"code":"260403J1301R2","reason":"x"}]}`,
		"同编号两条":        `{"version":1,"units":[],"blocked":[{"code":"260403J1301","reason":"a"},{"code":"260403J1301","reason":"b"}]}`,
		"条目里有不认识的字段":   `{"version":1,"units":[],"blocked":[{"code":"260403J1301","reason":"a","extra":1}]}`,
		"blocked 不是数组": `{"version":1,"units":[],"blocked":{"code":"260403J1301"}}`,
	} {
		if _, err := PFLoadConfiguredPaths(pfcfgFile(t, body)); err == nil {
			t.Errorf("%s：应返回 error", name)
		}
	}
}
