package projsync

// T8 meta 目录文件格式测试：roots.json、coordinator.json、状态文件、请求文件。
// Python 以后读写同样的格式，这里的往返和拒绝用例就是格式的规格。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPFRemoteRootsFile(t *testing.T) {
	meta := t.TempDir()
	if _, err := PFRemoteLoadRoots(meta); err == nil || !strings.Contains(err.Error(), "没有部署") {
		t.Fatalf("没有 roots.json 应报没部署: %v", err)
	}
	pfrtNoErr(t, PFRemoteWriteRoots(meta, []string{"/home/u/proj", "/data/p2"}))
	got, err := PFRemoteLoadRoots(meta)
	pfrtNoErr(t, err)
	if len(got) != 2 || got[0] != "/home/u/proj" || got[1] != "/data/p2" {
		t.Fatalf("%v", got)
	}
	for name, roots := range map[string][]string{
		"空":     {},
		"相对路径":  {"proj"},
		"不干净":   {"/a/../b"},
		"尾斜杠":   {"/a/b/"},
		"根目录":   {"/"},
		"重复":    {"/a", "/a"},
		"含 NUL": {"/a\x00b"},
	} {
		if err := PFRemoteWriteRoots(meta, roots); err == nil {
			t.Errorf("%s: 写入应被拒绝", name)
		}
	}
	// 读：版本不对、未知字段、多余内容、不合格的内容
	p := filepath.Join(meta, PFRemoteRootsFile)
	for name, body := range map[string]string{
		"版本":     `{"version":2,"roots":["/a"]}`,
		"未知字段":   `{"version":1,"roots":["/a"],"x":1}`,
		"多余内容":   `{"version":1,"roots":["/a"]} {}`,
		"相对路径":   `{"version":1,"roots":["a"]}`,
		"不是JSON": `roots`,
	} {
		pfrtNoErr(t, os.WriteFile(p, []byte(body), 0o644))
		if _, err := PFRemoteLoadRoots(meta); err == nil {
			t.Errorf("%s: 读取应失败", name)
		}
	}
	// 符号链接和目录不当作 roots.json
	pfrtNoErr(t, os.Remove(p))
	other := filepath.Join(meta, "别的.json")
	pfrtNoErr(t, os.WriteFile(other, []byte(`{"version":1,"roots":["/a"]}`), 0o644))
	pfrtNoErr(t, os.Symlink(other, p))
	if _, err := PFRemoteLoadRoots(meta); err == nil {
		t.Error("roots.json 是符号链接应拒绝")
	}
}

func TestPFRemoteLeaseFile(t *testing.T) {
	meta := t.TempDir()
	if l, err := pfrReadLease(meta); l != nil || err != nil {
		t.Fatalf("没有租约文件应是 (nil, nil): %v %v", l, err)
	}
	exp := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC).Format(time.RFC3339)
	pfrtNoErr(t, pfrWriteLease(meta, PFRemoteLease{ID: "mac-A", Host: "主机", Expires: exp}))
	l, err := pfrReadLease(meta)
	pfrtNoErr(t, err)
	if l.Version != 1 || l.ID != "mac-A" || l.Host != "主机" || !l.expiresAt().Equal(time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("%+v", l)
	}
	b, _ := os.ReadFile(filepath.Join(meta, PFRemoteLeaseFile))
	if string(b) != `{"version":1,"id":"mac-A","host":"主机","expires":"2026-10-27T01:02:03Z"}`+"\n" {
		t.Fatalf("租约文件的格式（Python 要读）: %s", b)
	}
	for _, bad := range []PFRemoteLease{
		{ID: "带 空格", Expires: exp}, {ID: "a", Expires: "明天"}, {ID: "", Expires: exp}, {ID: "a", Host: "x\ny", Expires: exp},
	} {
		if err := pfrWriteLease(meta, bad); err == nil {
			t.Errorf("%+v 应被拒绝", bad)
		}
	}
}

func TestPFRemoteStatusValidation(t *testing.T) {
	ok := PFRemoteStatus{Version: 1, Unit: "260403J1001", RoundAt: "2026-10-27T12:00:00Z",
		PendingToUbuntu: []PFRemotePending{{Path: "a/b.docx", Size: 1, Sha: pfrtSHA("x"), Reason: PFRemoteReasonQueued}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	neg := int64(-1)
	cases := map[string]func(*PFRemoteStatus){
		"版本":              func(s *PFRemoteStatus) { s.Version = 2 },
		"单元键小写":           func(s *PFRemoteStatus) { s.Unit = "260403j1001" },
		"时间不对":            func(s *PFRemoteStatus) { s.RoundAt = "昨天" },
		"ubuntu_rel 含 ..": func(s *PFRemoteStatus) { s.UbuntuRel = "../x" },
		"路径含 ..":          func(s *PFRemoteStatus) { s.PendingToUbuntu[0].Path = "../x" },
		"路径绝对": func(s *PFRemoteStatus) {
			s.PendingToMac = []PFRemotePending{{Path: "/x", Reason: PFRemoteReasonQueued}}
		},
		"大小为负":       func(s *PFRemoteStatus) { s.PendingToUbuntu[0].Size = -1 },
		"ub_size 为负": func(s *PFRemoteStatus) { s.PendingToUbuntu[0].UbSize = &neg },
		"sha 不对":     func(s *PFRemoteStatus) { s.PendingToUbuntu[0].Sha = "abc" },
		"原因不认识":      func(s *PFRemoteStatus) { s.PendingToUbuntu[0].Reason = "随便" },
		"冲突胜者不认识":    func(s *PFRemoteStatus) { s.Conflicts = []PFRemoteConflict{{Path: "a", ConflictPath: "b", Winner: "x"}} },
		"冲突副本路径不对": func(s *PFRemoteStatus) {
			s.Conflicts = []PFRemoteConflict{{Path: "a", ConflictPath: "../b", Winner: "mac"}}
		},
	}
	for name, mod := range cases {
		s := ok
		s.PendingToUbuntu = append([]PFRemotePending(nil), ok.PendingToUbuntu...)
		mod(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
		if err := PFRemoteWriteStatus(t.TempDir(), s); err == nil {
			t.Errorf("%s: 写入应被拒绝", name)
		}
	}
	// 读：读不懂、unit 与文件名不符
	meta := t.TempDir()
	dir := filepath.Join(meta, PFRemoteStatusName)
	pfrtNoErr(t, os.MkdirAll(dir, 0o755))
	if _, err := PFRemoteReadStatus(meta, "260403J1001"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("不存在应能用 fs.ErrNotExist 认出: %v", err)
	}
	pfrtNoErr(t, os.WriteFile(filepath.Join(dir, "260403J1001.json"), []byte(`{"version":1,"unit":"260403J1002","pending_to_ubuntu":[],"pending_to_mac":[],"conflicts":[]}`), 0o644))
	if _, err := PFRemoteReadStatus(meta, "260403J1001"); err == nil || !strings.Contains(err.Error(), "unit") {
		t.Fatalf("文件里的 unit 与文件名不符应拒绝: %v", err)
	}
	pfrtNoErr(t, os.WriteFile(filepath.Join(dir, "260403J1001.json"), []byte(`{"version":1,"unit":"260403J1001","bogus":1}`), 0o644))
	if _, err := PFRemoteReadStatus(meta, "260403J1001"); err == nil {
		t.Fatal("未知键应拒绝")
	}
	if _, err := PFRemoteReadStatus(meta, "../x"); err == nil {
		t.Fatal("单元键不合法应拒绝")
	}
	// _mac.json
	mac := PFRemoteMacStatus{Version: 1, MacSeenAt: "2026-10-27T12:00:00Z", Network: PFRemoteNetGood}
	pfrtNoErr(t, PFRemoteWriteMacStatus(meta, mac))
	if got, err := PFRemoteReadMacStatus(meta); err != nil || got != mac {
		t.Fatalf("%+v %v", got, err)
	}
	for name, m := range map[string]PFRemoteMacStatus{
		"网络类别": {Version: 1, MacSeenAt: "2026-10-27T12:00:00Z", Network: "wifi"},
		"没有时间": {Version: 1, Network: PFRemoteNetGood},
		"版本":   {Version: 3, MacSeenAt: "2026-10-27T12:00:00Z", Network: PFRemoteNetGood},
	} {
		if err := PFRemoteWriteMacStatus(meta, m); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
	}
}

func TestPFRemoteRequestFiles(t *testing.T) {
	meta := t.TempDir()
	now := time.Date(2026, 10, 27, 4, 0, 0, 0, time.UTC)
	if g, b, more, err := PFRemoteListRequests(meta, 0); err != nil || len(g) != 0 || g == nil || len(b) != 0 || more {
		t.Fatalf("没有目录应是空列表: %v %v %v %v", g, b, more, err)
	}
	// 往返：文件名 <键>-<UTC 时间>-<8 位十六进制>.json
	n1, err := PFRemoteWriteRequest(meta, PFRemoteRequest{Unit: "260403J1002", Paths: []string{"报告/a.docx", "b.txt"}, Priority: PFRemotePrioUser}, now)
	pfrtNoErr(t, err)
	if !strings.HasPrefix(n1, "260403J1002-20261027T040000Z-") || !strings.HasSuffix(n1, ".json") || len(n1) != len("260403J1002-20261027T040000Z-00000000.json") {
		t.Fatalf("文件名: %s", n1)
	}
	n2, err := PFRemoteWriteRequest(meta, PFRemoteRequest{Kind: PFRemoteReqTouched, Unit: "260403J1001", Priority: PFRemotePrioUser}, now.Add(-time.Hour))
	pfrtNoErr(t, err)
	n3, err := PFRemoteWriteRequest(meta, PFRemoteRequest{Unit: "260403J1001", Paths: []string{"c"}, Priority: PFRemotePrioUser}, now.Add(time.Hour))
	pfrtNoErr(t, err)
	raw, _ := os.ReadFile(filepath.Join(meta, PFRemoteRequestName, n1))
	if !strings.Contains(string(raw), `"version":1`) || !strings.Contains(string(raw), `"created":"2026-10-27T04:00:00Z"`) || !strings.Contains(string(raw), `"priority":"user"`) {
		t.Fatalf("请求文件内容: %s", raw)
	}
	// 不该被列出的：原子写留下的临时文件、名字不符合格式的、名字符合格式但是目录的
	d := filepath.Join(meta, PFRemoteRequestName)
	pfrtNoErr(t, os.WriteFile(filepath.Join(d, n1+".tmp-123"), []byte("x"), 0o644))
	pfrtNoErr(t, os.WriteFile(filepath.Join(d, "说明.txt"), []byte("x"), 0o644))
	pfrtNoErr(t, os.Mkdir(filepath.Join(d, "260403J1003-20261027T050000Z-aaaaaaaa.json"), 0o755))
	// 读不懂的：报进 bad，不删
	badUnitMismatch := "260403J1004-20261027T060000Z-bbbbbbbb.json"
	pfrtNoErr(t, os.WriteFile(filepath.Join(d, badUnitMismatch), []byte(`{"version":1,"unit":"260403J1005","paths":["a"],"priority":"user","created":"2026-10-27T06:00:00Z"}`), 0o644))
	badUnknown := "260403J1004-20261027T070000Z-cccccccc.json"
	pfrtNoErr(t, os.WriteFile(filepath.Join(d, badUnknown), []byte(`{"version":1,"unit":"260403J1004","paths":["a"],"priority":"user","created":"2026-10-27T06:00:00Z","x":1}`), 0o644))

	good, bad, more, err := PFRemoteListRequests(meta, 0)
	pfrtNoErr(t, err)
	if more || len(good) != 3 || len(bad) != 2 {
		t.Fatalf("good=%d bad=%d more=%v", len(good), len(bad), more)
	}
	// 按文件名里的时间升序（不是按单元键）
	if good[0].Name != n2 || good[1].Name != n1 || good[2].Name != n3 {
		t.Fatalf("顺序: %s %s %s", good[0].Name, good[1].Name, good[2].Name)
	}
	if good[1].Request.Unit != "260403J1002" || good[1].Request.Paths[1] != "b.txt" || good[0].Request.Kind != PFRemoteReqTouched {
		t.Fatalf("内容: %+v", good)
	}
	if bad[0].Name != badUnitMismatch || bad[1].Name != badUnknown {
		t.Fatalf("坏的: %+v", bad)
	}
	// 限量：好的加坏的最多 limit 个，其余标 more
	good, bad, more, err = PFRemoteListRequests(meta, 2)
	pfrtNoErr(t, err)
	if !more || len(good)+len(bad) != 2 {
		t.Fatalf("限量: good=%d bad=%d more=%v", len(good), len(bad), more)
	}

	// 取走：好的返回内容并删除，不在的记 missing，坏的也删并报告
	taken, missing, badF, err := PFRemoteTakeRequests(meta, []string{n1, "260403J1001-20250101T000000Z-00000000.json", badUnitMismatch})
	pfrtNoErr(t, err)
	if len(taken) != 1 || taken[0].Name != n1 || len(missing) != 1 || len(badF) != 1 || badF[0].Name != badUnitMismatch {
		t.Fatalf("取走: %+v %v %+v", taken, missing, badF)
	}
	for _, n := range []string{n1, badUnitMismatch} {
		if _, err := os.Lstat(filepath.Join(d, n)); !os.IsNotExist(err) {
			t.Errorf("%s 应已删除", n)
		}
	}
	if _, err := os.Lstat(filepath.Join(d, n2)); err != nil {
		t.Error("没点名的请求不该被删")
	}
	// 名字不合格：整个调用拒绝，一个都不删
	if _, _, _, err := PFRemoteTakeRequests(meta, []string{n2, "../" + n3}); err == nil {
		t.Fatal("名字不合格应拒绝")
	}
	if _, err := os.Lstat(filepath.Join(d, n2)); err != nil {
		t.Fatal("名字不合格的调用不该删任何文件")
	}
}

func TestPFRemoteRequestValidation(t *testing.T) {
	ok := PFRemoteRequest{Version: 1, Unit: "260403J1001", Paths: []string{"a"}, Priority: "user", Created: "2026-10-27T12:00:00Z"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	many := make([]string, PFRemoteMaxPaths+1)
	for i := range many {
		many[i] = fmt.Sprint("f", i)
	}
	cases := map[string]func(*PFRemoteRequest){
		"版本":        func(r *PFRemoteRequest) { r.Version = 2 },
		"单元键":       func(r *PFRemoteRequest) { r.Unit = "x y" },
		"种类":        func(r *PFRemoteRequest) { r.Kind = "delete" },
		"fetch 没路径": func(r *PFRemoteRequest) { r.Paths = nil },
		"路径太多":      func(r *PFRemoteRequest) { r.Paths = many },
		"路径越界":      func(r *PFRemoteRequest) { r.Paths = []string{"../x"} },
		"优先级":       func(r *PFRemoteRequest) { r.Priority = "high" },
		"没有时间":      func(r *PFRemoteRequest) { r.Created = "" },
	}
	for name, mod := range cases {
		r := ok
		mod(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
	}
	// touched 可以没有路径（整个单元）
	r := ok
	r.Kind, r.Paths = PFRemoteReqTouched, nil
	if err := r.Validate(); err != nil {
		t.Fatalf("touched 不带路径应可以: %v", err)
	}
	// 文件名不合格
	for _, name := range []string{"x.json", "260403J1001-20261027T040000Z-XYZ.json", "260403j1001-20261027T040000Z-aaaaaaaa.json"} {
		if _, err := PFRemoteParseRequest(name, []byte(`{"version":1,"unit":"260403J1001","paths":["a"],"priority":"user","created":"2026-10-27T12:00:00Z"}`)); err == nil {
			t.Errorf("文件名 %q 应被拒绝", name)
		}
	}
}
