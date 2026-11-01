package wgtund

// projfiles_settingsmode_test.go：hosts.json 没写 projfiles 段时的模式（配置只从 XXYI 设置窗口改）——
// 服务与接口照样起，host/remote_root/remote_meta 每轮从设置文件取，总开关是设置里的 enabled（缺省关）；
// 缺项/不合法这一轮不跑、原因原文进状态；改 host 走已有的整套重建切换。远端一律假 Runner，不连真机器。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pfNoSectionConf 造一份没有 projfiles 段的 fileConf；hosts 里放一台合规（ubuntu）与一台没私钥的（macmini）。
func pfNoSectionConf(t *testing.T) *fileConf {
	t.Helper()
	return &fileConf{KnownHosts: "/kh", Hosts: []hostConf{
		{Name: "ubuntu", Addr: "127.0.0.1:22", User: "chenwy", IdentityFiles: []string{"/id"}},
		{Name: "ubuntu2", Addr: "127.0.0.1:22", User: "chenwy", IdentityFiles: []string{"/id2"}},
		{Name: "macmini", Addr: "127.0.0.1:22", User: "chenwy"},
	}}
}

// pfSvcNoSection 是不连真机器的没段服务：假 Runner、假 SFTP、假时钟、临时目录。
func pfSvcNoSection(t *testing.T) (*projfilesSvc, *pfClock, string) {
	t.Helper()
	prep, err := prepareProjfiles(pfNoSectionConf(t))
	if err != nil {
		t.Fatalf("没这段不该报错: %v", err)
	}
	if prep == nil || !prep.fromSettings || prep.enabled {
		t.Fatalf("应是 fromSettings 且缺省关: %+v", prep)
	}
	dir := t.TempDir()
	prep.metaDir = filepath.Join(dir, "projfiles")
	prep.settingsPath = filepath.Join(dir, "projfiles-settings.json")
	prep.statusPath = filepath.Join(dir, "projfiles", "status", "_round.json")
	clock := &pfClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	s := newProjfilesSvc(prep, nil)
	s.now = clock.now
	s.runner = &pfFakeRunner{}
	s.attachSFTP = func() error { return nil }
	return s, clock, dir
}

// pfGoodSettings 是设置齐全的一份设置（没这段模式）。
func pfGoodSettings() map[string]any {
	return map[string]any{"enabled": true, "host": "ubuntu",
		"remote_root": "/srv/XXYI-sync", "remote_meta": "/srv/.xxyi/projfiles"}
}

func TestProjfilesNoSectionFullSettingsRuns(t *testing.T) {
	s, _, dir := pfSvcNoSection(t)
	var rounds pfRounds
	rounds.hook(s)
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), pfGoodSettings())
	s.tick(context.Background(), "timer")
	if rounds.len() != 1 {
		t.Fatalf("设置齐全且 enabled 应跑一轮: %d", rounds.len())
	}
	st := s.status()
	if !st.Enabled || !strings.Contains(st.LastAction, "跑了：") {
		t.Errorf("状态应记跑了: %+v", st)
	}
	if st.Host != "ubuntu" || st.HostFrom != "settings" {
		t.Errorf("host 应报设置里的并标来源: %s/%s", st.Host, st.HostFrom)
	}
	if st.RemoteRoot != "/srv/XXYI-sync" || st.RemoteRootFrom != "settings" ||
		st.RemoteMeta != "/srv/.xxyi/projfiles" || st.RemoteMetaFrom != "settings" {
		t.Errorf("生效三件套与来源不对: %s/%s %s/%s", st.RemoteRoot, st.RemoteRootFrom, st.RemoteMeta, st.RemoteMetaFrom)
	}
	if st.ConfigGap != "" || st.CoordID == "" {
		t.Errorf("齐全时不该有缺口、coord 照规则补: %q %q", st.ConfigGap, st.CoordID)
	}
	if s.activeHost() != "ubuntu" {
		t.Errorf("绑定的主机应是 ubuntu: %s", s.activeHost())
	}
}

func TestProjfilesNoSectionGapsBlockRound(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(map[string]any)
		wantGap string
	}{
		{"设置里 enabled=false", func(m map[string]any) { m["enabled"] = false }, ""},
		{"缺 host", func(m map[string]any) { delete(m, "host") }, "设置里没有 host"},
		{"host 不在 hosts", func(m map[string]any) { m["host"] = "ghost" }, `设置里的 host "ghost" 不在 hosts.json 里`},
		{"host 没有私钥", func(m map[string]any) { m["host"] = "macmini" }, `设置里的 host "macmini" 没有 identity_files，SSH 无法选私钥`},
		{"缺 remote_root", func(m map[string]any) { delete(m, "remote_root") }, "设置里没有 remote_root"},
		{"缺 remote_meta", func(m map[string]any) { delete(m, "remote_meta") }, "设置里没有 remote_meta"},
		{"remote_meta 不合规", func(m map[string]any) { m["remote_meta"] = "/srv/中文" }, "设置里没有 remote_meta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, dir := pfSvcNoSection(t)
			var rounds pfRounds
			rounds.hook(s)
			m := pfGoodSettings()
			tc.mut(m)
			pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), m)
			s.tick(context.Background(), "timer")
			if rounds.len() != 0 {
				t.Fatalf("这一轮不该跑: %+v", rounds.all())
			}
			st := s.status()
			if tc.wantGap == "" {
				if !strings.Contains(st.LastAction, "enabled=false") {
					t.Errorf("enabled=false 应报原文: %q", st.LastAction)
				}
				return
			}
			if st.ConfigGap != tc.wantGap {
				t.Errorf("config_gap 应原文 %q，得到 %q", tc.wantGap, st.ConfigGap)
			}
			if st.LastAction != "没跑："+tc.wantGap {
				t.Errorf("last_action 应原文「没跑：%s」，得到 %q", tc.wantGap, st.LastAction)
			}
			if s.isRunning() || s.switchPending() {
				t.Errorf("缺项不该占轮次或进切换期")
			}
		})
	}
}

func TestProjfilesNoSectionMissingFileDefaultDisabled(t *testing.T) {
	s, _, _ := pfSvcNoSection(t)
	var rounds pfRounds
	rounds.hook(s)
	s.tick(context.Background(), "timer") // 设置文件不存在
	if rounds.len() != 0 {
		t.Fatalf("没有设置文件不该跑: %+v", rounds.all())
	}
	st := s.status()
	if st.Enabled {
		t.Errorf("没这段时设置文件缺省必须关: %+v", st)
	}
	if st.ConfigGap != "设置里没有 host" {
		t.Errorf("应报缺 host，得到 %q", st.ConfigGap)
	}
}

func TestProjfilesNoSectionSettingsEnabledKeyOnly(t *testing.T) {
	// projfilesDefaults 的 enabled 是 true：没这段时没写 enabled 键必须落回 false
	s, _, dir := pfSvcNoSection(t)
	var rounds pfRounds
	rounds.hook(s)
	m := pfGoodSettings()
	delete(m, "enabled")
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), m)
	s.tick(context.Background(), "timer")
	if rounds.len() != 0 {
		t.Fatalf("没写 enabled 键缺省关，不该跑")
	}
	if s.status().Enabled {
		t.Error("enabled 缺省应 false")
	}
}

func TestProjfilesNoSectionHostSwitchRebuilds(t *testing.T) {
	s, _, dir := pfSvcNoSection(t)
	var rounds pfRounds
	rounds.hook(s)
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), pfGoodSettings())
	s.tick(context.Background(), "timer")
	if rounds.len() != 1 || s.activeHost() != "ubuntu" {
		t.Fatalf("第一轮应在 ubuntu 上跑: %d %s", rounds.len(), s.activeHost())
	}
	// 改 host：走「切换期 → 占住轮次后整套重建」，重建期间旧一套先关
	m := pfGoodSettings()
	m["host"] = "ubuntu2"
	m["remote_meta"] = "/srv/.xxyi/projfiles2"
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), m)
	st2, _ := s.loadSettings() // 设置已改、tick 还没跑：按磁盘上的新设置判定要重建
	if !s.needRebuild(st2) {
		t.Fatal("改了 host/remote_meta 该判定要重建")
	}
	s.tick(context.Background(), "timer")
	if rounds.len() != 2 {
		t.Fatalf("改 host 后照常跑: %d", rounds.len())
	}
	if s.activeHost() != "ubuntu2" || s.switchPending() {
		t.Errorf("应已在 ubuntu2 上重建完: %s busy=%v", s.activeHost(), s.switchPending())
	}
	s.mu.Lock()
	builtMeta := s.builtMeta
	s.mu.Unlock()
	if builtMeta != "/srv/.xxyi/projfiles2" {
		t.Errorf("remote_meta 也该跟着换: %s", builtMeta)
	}
	st := s.status()
	if st.ActiveHost != "ubuntu2" || st.Host != "ubuntu2" || st.ConfigGap != "" {
		t.Errorf("状态不对: %+v", st)
	}
	// 重建后再改回不合法的 host：不跑、留在原主机上
	delete(m, "host")
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), m)
	s.tick(context.Background(), "timer")
	if rounds.len() != 2 {
		t.Fatalf("host 缺失后不该再跑: %d", rounds.len())
	}
	if s.activeHost() != "ubuntu2" || s.status().ConfigGap != "设置里没有 host" {
		t.Errorf("不合法设置不该动已建成的一套: %s %+v", s.activeHost(), s.status())
	}
}

func TestProjfilesSectionModeIgnoresSettingsHost(t *testing.T) {
	// 有 projfiles 段：段里的值优先，设置里的 host/remote_meta 不越权、也不报缺口
	s, _, dir := pfSvcT(t, nil)
	var rounds pfRounds
	rounds.hook(s)
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"),
		map[string]any{"enabled": true, "host": "ghost", "remote_meta": "/srv/中文"})
	s.tick(context.Background(), "timer")
	if rounds.len() != 1 {
		t.Fatalf("设置里的坏 host 不该挡住有段模式: %+v", rounds.all())
	}
	st := s.status()
	if st.Host != "ubuntu" || st.HostFrom != "hosts" || st.RemoteMeta != "/srv/.xxyi/projfiles" || st.RemoteMetaFrom != "hosts" {
		t.Errorf("有段时三件套应仍来自段: %+v", st)
	}
	if st.ConfigGap != "" {
		t.Errorf("有段时不该有 config_gap: %q", st.ConfigGap)
	}
	if st.Enabled != true {
		t.Errorf("有段时总开关仍看段: %+v", st)
	}
}
