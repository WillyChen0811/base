package projsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 项目直接放在工作目录第 1 层（rel 只有一段，没有年份层）时，登记校验、Ubuntu 侧发现单元、
// Ubuntu 侧按 rel 建目录、改名/取消的路径都要走得通（XXYI「桌面上未同步」列表 2026-11 起也列第 1 层的项目）。

const (
	flatCode = "260403J1011"
	flatRel  = "260403J1011-单层项目"
)

func TestPFFlat_ValidateUnit(t *testing.T) {
	u := PFUnitRecord{Code: flatCode, Rel: flatRel, State: PFUnitSyncing,
		LinkPath: "/Users/u/Desktop/Projects/" + flatRel, SyncPath: "/Users/u/XXYI-sync/" + flatRel, StartedAt: time.Now()}
	if err := PFValidateUnit(u); err != nil {
		t.Fatalf("单段 rel 应是合法登记：%v", err)
	}
}

func TestPFFlat_DiscoverFindsFirstLayerUnit(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{flatRel, "2026/260403J1012-年份下", "杂项"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, u := range res.Units {
		got[u.Rel] = u.Code
	}
	if got[flatRel] != flatCode || got["2026/260403J1012-年份下"] != "260403J1012" || len(got) != 2 {
		t.Fatalf("应同时发现第 1 层和年份层下的单元：%v", got)
	}
}

func TestPFFlat_RemoteApplyUnitMkdirFirstLayer(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	u := PFRemoteUnit{Code: flatCode, Rel: flatRel}
	res, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: u}, []PFRemoteStep{{Op: PFRemoteOpUnitMkdir}, {Op: PFRemoteOpMkdir, Path: "资料"}})
	pfrtNoErr(t, err)
	for _, r := range res.Results {
		pfrtDone(t, r)
	}
	if fi, err := os.Lstat(filepath.Join(f.root, flatRel, "资料")); err != nil || !fi.IsDir() {
		t.Fatalf("Ubuntu 根下第 1 层应建出 <root>/%s/资料：%v", flatRel, err)
	}
}

// 项目放在非年份的文件夹下一层（rel = 「客户资料/<项目>」）：XXYI 列表也列这种目录。
// 上级「客户资料」认不出编号、不以「.」开头，登记校验、搬运器来回、Ubuntu 发现与按 rel 建目录都要走得通。

const (
	grpCode = "260403J1102"
	grpRel  = "客户资料/260403J1102-华瑞补充测试"
)

func TestPFGroup_ValidateUnit(t *testing.T) {
	u := PFUnitRecord{Code: grpCode, Rel: grpRel, State: PFUnitSyncing,
		LinkPath: "/Users/u/Desktop/Projects/" + grpRel, SyncPath: "/Users/u/XXYI-sync/" + grpRel, StartedAt: time.Now()}
	if err := PFValidateUnit(u); err != nil {
		t.Fatalf("上级是普通文件夹的两段 rel 应是合法登记：%v", err)
	}
}

func TestPFGroup_MoveStartStopRoundTrip(t *testing.T) {
	e := newPFUMEnv(t)
	p := e.project(grpRel)
	want := e.want(t, p)
	e.mustStart(t, grpRel)
	if where := e.assertOneCopy(t, grpRel, want, false); where != "link" {
		t.Fatalf("开始同步后桌面应是链接：%s", where)
	}
	if tgt, err := os.Readlink(p); err != nil || tgt != filepath.Join(e.syn, filepath.FromSlash(grpRel)) {
		t.Errorf("链接应指向 <同步目录>/客户资料/<项目>：%q %v", tgt, err)
	}
	e.wantUnit(t, grpCode, PFUnitSyncing)
	e.mustStop(t, grpCode)
	if where := e.assertOneCopy(t, grpRel, want, false); where != "desktop" {
		t.Fatalf("取消同步应回到桌面：%s", where)
	}
	if _, err := os.Lstat(filepath.Join(e.syn, "客户资料")); err == nil {
		t.Errorf("取消后同步目录里空了的「客户资料」应被清掉")
	}
	if _, err := os.Lstat(e.syn); err != nil {
		t.Errorf("同步目录本身不能被删：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(e.work, "客户资料")); err != nil {
		t.Errorf("桌面上的「客户资料」文件夹不能被删：%v", err)
	}
}

func TestPFGroup_DiscoverAndRemoteApply(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(grpRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := PFDiscoverUnits(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Units) != 1 || res.Units[0].Rel != grpRel || res.Units[0].Code != grpCode {
		t.Fatalf("应发现「上级文件夹/项目」这个单元：%+v", res.Units)
	}
	f := newPFRTFix(t)
	r, err := f.cl.Apply(context.Background(), PFRemoteApplyOptions{Unit: PFRemoteUnit{Code: grpCode, Rel: grpRel}},
		[]PFRemoteStep{{Op: PFRemoteOpUnitMkdir}, {Op: PFRemoteOpMkdir, Path: "资料"}})
	pfrtNoErr(t, err)
	for _, x := range r.Results {
		pfrtDone(t, x)
	}
	if fi, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(grpRel), "资料")); err != nil || !fi.IsDir() {
		t.Fatalf("Ubuntu 根下应建出 <root>/%s/资料：%v", grpRel, err)
	}
}
