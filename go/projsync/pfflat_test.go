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
