//go:build itest

package itest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// ── 场景专用小工具 ──────────────────────────────────────

// schedUnit 取报告里某编号的单元条目（Stopped/not_done 的没有 Result，所以返回条目本身而不是 unitResult）。
func schedUnit(rep *projsync.PFSchedReport, code string) *projsync.PFSchedUnitReport {
	for i := range rep.Units {
		if rep.Units[i].Code == code {
			return &rep.Units[i]
		}
	}
	return nil
}

// lstatKind 描述本机一个路径的形态：missing / dir / link / file。
func lstatKind(t *testing.T, p string) string {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		t.Fatalf("Lstat %s 失败：%v", p, err)
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return "link"
	case fi.IsDir():
		return "dir"
	default:
		return "file"
	}
}

// ubTool 探测 Ubuntu 上有什么 sqlite 工具（python3 优先，其次 sqlite3，都没有返回空串）。
func (c *cluster) ubTool(ctx context.Context) string {
	c.t.Helper()
	out, _, _, err := c.env.runner.Run(ctx, "command -v python3 || command -v sqlite3 || true", nil)
	if err != nil {
		c.t.Fatalf("探测 Ubuntu sqlite 工具失败：%v", err)
	}
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
}

// ubSQLiteDump 在 Ubuntu 上打开一个 sqlite 文件（只读），打 PRAGMA integrity_check + 版本列表；
// 没有可用工具时返回 ""（调用方降级为字节级比对并在报告里说明）。
func (c *cluster) ubSQLiteDump(ctx context.Context, path string) string {
	c.t.Helper()
	tool := c.ubTool(ctx)
	switch {
	case strings.HasSuffix(tool, "python3"):
		stdout, stderr, exit, err := c.env.runner.Run(ctx, "python3 -c '"+pyRoDump+"' "+quoteShell(path), nil)
		if err != nil || exit != 0 {
			c.t.Fatalf("Ubuntu python3 打开台账失败（exit=%d）：%s\n%s", exit, err, stderr)
		}
		return string(stdout)
	case strings.HasSuffix(tool, "sqlite3"):
		stdout, stderr, exit, err := c.env.runner.Run(ctx,
			"sqlite3 "+quoteShell(path+"?mode=ro")+" 'PRAGMA integrity_check; SELECT v FROM versions ORDER BY v;'", nil)
		if err != nil || exit != 0 {
			c.t.Fatalf("Ubuntu sqlite3 打开台账失败（exit=%d）：%s\n%s", exit, err, stderr)
		}
		return string(stdout)
	}
	return ""
}

// macSQLiteDump 用本机 python3 打开台账（创建/校验都用它；python3 缺时 t.Skip——testenv.json 声明宿主机有 3.12）。
func macSQLiteDump(t *testing.T, path string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("本机没有 python3，无法造/验真实 sqlite 台账：%v", err)
	}
	cmd := exec.Command(python, "-c", pyRoDump, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("本机 python3 打开 %s 失败：%v\n%s", path, err, out)
	}
	return string(out)
}

// macLedgerWrite 用本机 python3 建/追加台账一对（sqlite + json）。
func macLedgerWrite(t *testing.T, db, js string, vers []string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("本机没有 python3，无法造真实 sqlite 台账：%v", err)
	}
	args := append([]string{"-c", pyWriteLedger, db, js}, vers...)
	out, err := exec.Command(python, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("本机 python3 写台账失败：%v\n%s", err, out)
	}
}

// pyWriteLedger：建表插版本、按库内容重写 json 索引（照 T10 夹具的 schema_version/versions 结构）。
const pyWriteLedger = `import sqlite3, sys, json
db, js = sys.argv[1], sys.argv[2]
con = sqlite3.connect(db)
con.execute("CREATE TABLE IF NOT EXISTS versions(v TEXT PRIMARY KEY, note TEXT)")
for v in sys.argv[3:]:
    con.execute("INSERT OR REPLACE INTO versions(v, note) VALUES(?, ?)", (v, "版本 " + v))
con.commit()
rows = con.execute("SELECT v, note FROM versions ORDER BY v").fetchall()
con.close()
meta = {"schema_version": 1, "versions": [{"v": r[0], "note": r[1]} for r in rows]}
open(js, "w", encoding="utf-8").write(json.dumps(meta, ensure_ascii=False))
`

// pyRoDump：只读打开，打 integrity_check 和版本列表。
const pyRoDump = `import sqlite3, sys
con = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
print(con.execute("PRAGMA integrity_check").fetchone()[0])
for r in con.execute("SELECT v FROM versions ORDER BY v"):
    print(r[0])
`

const (
	ledgerDB = "历史版本/档案台账.sqlite3"
	ledgerJS = "历史版本/档案索引.json"
)

// ── S5 整项目移出再加回（G1）──────────────────────────

// S5 整项目移出再加回：对齐后 Mac 把整个项目挪出 sync_dir → 一轮后 Ubuntu 不被删、报告有说明（G1/已停止同步）；
// 挪回来 → 下一轮恢复正常、无大量重传。Ubuntu 端整个项目目录不见了 → Mac 不删（G1 停下，不写任何东西）。
func TestItestS5UnitMovedOutAndBack(t *testing.T) {
	c := newCluster(t)
	filesA := map[string][]byte{
		"大纲.txt":     []byte("大纲\n"),
		"素材/图一.png": []byte("png\n"),
		"素材/图二.png": []byte("png2\n"),
	}
	c.startUnit(relA, filesA)
	c.round(true)

	// Mac 把整个项目挪出同步目录（挪到 work/sync 之外的暂存处）。
	macSnap := c.macFilesOf(relA)
	ubSnap := c.ubFilesOf(relA)
	stash := filepath.Join(filepath.Dir(c.sync), "移出暂存")
	if err := os.MkdirAll(stash, 0o755); err != nil {
		t.Fatalf("建暂存处失败：%v", err)
	}
	if err := os.Rename(filepathJoin(c.sync, relA), filepathJoin(stash, relA)); err != nil {
		t.Fatalf("Mac 挪出项目失败：%v", err)
	}
	c.sleepQuiet()
	rep := c.round(true)

	if got := lstatKind(t, filepathJoin(c.sync, relA)); got != "missing" {
		t.Errorf("Mac 同步目录里的项目应该已被挪走，实际：%s", got)
	}
	if su := schedUnit(rep, codeA); su == nil {
		t.Errorf("报告里没有这个单元的条目：%s", jsonOf(rep.Units))
	} else if blob := jsonOf(su); !strings.Contains(blob, "G1") && !strings.Contains(blob, projsync.PFNoteStopped) {
		t.Errorf("报告里对「整项目不见了」没有 G1/已停止同步的说明：%s", blob)
	}
	if onlyMac, onlyUb, differ := cmpTree(ubSnap, c.ubFilesOf(relA)); len(onlyMac)+len(onlyUb)+len(differ) > 0 {
		t.Errorf("Mac 把项目挪走了，Ubuntu 那份不该被动：缺 %v 多 %v 不同 %v", onlyMac, onlyUb, differ)
	}

	// 挪回来 → 下一轮恢复正常、无大量重传。
	if err := os.Rename(filepathJoin(stash, relA), filepathJoin(c.sync, relA)); err != nil {
		t.Fatalf("Mac 挪回项目失败：%v", err)
	}
	c.sleepQuiet()
	rep = c.round(true)
	c.assertAligned(relA, rep)
	su := schedUnit(rep, codeA)
	if su == nil || su.Result == nil {
		t.Fatalf("挪回后单元没回到正常轮：units=%s", jsonOf(rep.Units))
	}
	res := su.Result
	if res.BytesUp > 0 || res.BytesDown > 0 {
		t.Errorf("挪回后不该有重传：bytes_up=%d bytes_down=%d done=%s", res.BytesUp, res.BytesDown, jsonOf(res.Done))
	}

	// Ubuntu 端整个项目目录不见了 → Mac 不删，G1 停下。
	if err := os.RemoveAll(filepathJoin(stash, relA)); err != nil {
		t.Fatalf("清暂存失败：%v", err)
	}
	c.env.sh(context.Background(), "rm -rf -- "+quoteShell(c.env.root+"/"+relA))
	macSnap = c.macFilesOf(relA)
	c.sleepQuiet()
	rep = c.round(true)
	if onlyMac, onlyUb, differ := cmpTree(macSnap, c.macFilesOf(relA)); len(onlyMac)+len(onlyUb)+len(differ) > 0 {
		t.Errorf("Ubuntu 项目目录不见了，Mac 这份不该被删/改：缺 %v 多 %v 不同 %v", onlyMac, onlyUb, differ)
	}
	suUb := schedUnit(rep, codeA)
	if suUb == nil {
		t.Fatalf("报告里没有这个单元的条目：%s", jsonOf(rep.Units))
	}
	hitG1 := false
	if suUb.Result != nil {
		for _, g := range suUb.Result.Gates {
			if g.Code == "G1" {
				hitG1 = true
			}
		}
	}
	if !hitG1 && !strings.Contains(jsonOf(suUb), "G1") {
		t.Errorf("报告里没有 G1 关卡说明：%s", jsonOf(suUb))
	}
	for _, p := range ubuntuPaths(t, c.env.sf, c.env.root) {
		if strings.HasPrefix(p, relA+"/") || p == relA {
			t.Errorf("G1 停下还不许写 Ubuntu，回收区之外冒出了 %s", p)
		}
	}
	t.Logf("S5 移出轮报告：%s", jsonOf(rep))
}

// ── S6 / S9 暂缓 ────────────────────────────────────────

// S6 暂缓期间删除不误传：热点网络下新增 10MB 文件 → 本轮暂缓（deferred + pending 有它，零上传）；
// Mac 把它删了；网络改回 good 再一轮 → Ubuntu 上不出现这个文件，也没有报错。
//
// 阈值：联调不造 100MB（缺省 MaxExpensive=20s≈5MB/s×20s），把 MaxExpensive 压到 1 毫秒，10MB 必超。
func TestItestS6DeferDuringExpensiveThenDeleted(t *testing.T) {
	c := newCluster(t, withMaxExpensive(time.Millisecond))
	c.startUnit(relA, map[string][]byte{"正文.txt": []byte("正文\n")})
	c.round(true)

	const mb10 = 10 << 20
	c.sch.SetNet(projsync.PFRemoteNetExpensive, time.Now())
	name := "热点大文件.bin"
	c.writeMacInSync(relA, map[string][]byte{name: bigFile(mb10)})
	c.sleepQuiet()
	rep := c.round(true)
	res := unitResult(rep, codeA)
	if res == nil {
		t.Fatalf("报告里没有单元结果：%s", jsonOf(rep.Units))
	}
	hit := false
	for _, d := range res.Deferred {
		if d.Path == name && d.Size == mb10 && d.Reason != "" {
			hit = true
		}
	}
	if !hit {
		t.Errorf("热点下 10MB 新文件应进暂缓清单：%s", jsonOf(res.Deferred))
	}
	if res.BytesUp != 0 {
		t.Errorf("暂缓那轮不该有上传：bytes_up=%d", res.BytesUp)
	}
	if _, ok := c.ubFilesOf(relA)[name]; ok {
		t.Errorf("暂缓的文件出现在 Ubuntu 上了")
	}
	if !strings.Contains(jsonOf(res.Status.PendingToUbuntu), name) {
		t.Errorf("Ubuntu status 的 pending_to_ubuntu 里应该有它：%s", jsonOf(res.Status.PendingToUbuntu))
	}

	// Mac 删掉它；网络改回 good → Ubuntu 上不出现、无报错。
	if err := os.Remove(filepathJoin(c.sync, relA, name)); err != nil {
		t.Fatalf("Mac 删文件失败：%v", err)
	}
	c.sleepQuiet()
	c.sch.SetNet(projsync.PFRemoteNetGood, time.Now())
	rep = c.round(true)
	if _, ok := c.ubFilesOf(relA)[name]; ok {
		t.Errorf("暂缓期间被删掉的文件不该在网好后补传到 Ubuntu")
	}
	res = unitResult(rep, codeA)
	if res != nil && len(res.Deferred) > 0 {
		t.Errorf("删掉后不该还挂着暂缓项：%s", jsonOf(res.Deferred))
	}
	c.assertAligned(relA, rep)
	t.Logf("S6 删除后报告：%s", jsonOf(rep))
}

// S9 暂缓与补传：热点下暂缓后不删，网络改回 good → 下一轮补传，两边一致。
func TestItestS9DeferThenResume(t *testing.T) {
	c := newCluster(t, withMaxExpensive(time.Millisecond))
	c.startUnit(relA, map[string][]byte{"正文.txt": []byte("正文\n")})
	c.round(true)

	const mb10 = 10 << 20
	c.sch.SetNet(projsync.PFRemoteNetExpensive, time.Now())
	name := "热点大文件.bin"
	content := bigFile(mb10)
	c.writeMacInSync(relA, map[string][]byte{name: content})
	c.sleepQuiet()
	rep := c.round(true)
	res := unitResult(rep, codeA)
	if res == nil || len(res.Deferred) == 0 {
		t.Fatalf("热点下没暂缓：%s", jsonOf(rep.Units))
	}

	c.sch.SetNet(projsync.PFRemoteNetGood, time.Now())
	rep = c.round(true)
	res = unitResult(rep, codeA)
	if res == nil {
		t.Fatalf("补传轮没有单元结果：%s", jsonOf(rep.Units))
	}
	if len(res.Deferred) > 0 {
		t.Errorf("网好后不该继续暂缓：%s", jsonOf(res.Deferred))
	}
	if res.BytesUp < mb10 {
		t.Errorf("补传轮 bytes_up=%d，应至少把 %d 字节传过去", res.BytesUp, mb10)
	}
	c.assertAligned(relA, rep)
}

// ── S16 写到一半（quiet_seconds）───────────────────────

// S16 写到一半：quiet 窗口设成 60 秒，刚写的文件本轮不传、报告有说明；mtime 拨到 90 秒前再一轮 → 传过去。
func TestItestS16HalfWritten(t *testing.T) {
	c := newCluster(t, withQuietSeconds(60))
	c.startUnit(relA, map[string][]byte{"说明.txt": []byte("说明\n")})
	c.round(true)

	name := "正在写的草稿.txt"
	content := []byte("写到一半的内容\n")
	c.writeMacInSync(relA, map[string][]byte{name: content}) // mtime=刚刚
	rep := c.round(true)

	if _, ok := c.ubFilesOf(relA)[name]; ok {
		t.Errorf("刚写的文件（quiet 窗口内）不该传到 Ubuntu")
	}
	blob := jsonOf(rep)
	if !strings.Contains(blob, name) {
		t.Errorf("报告里对这个文件没有交代（既没传也没说明）：%s", blob)
	}
	res := unitResult(rep, codeA)
	if res != nil {
		for _, d := range res.Done {
			if d.Path == name {
				t.Errorf("quiet 窗口内的文件出现在 Done 里：%s", jsonOf(res.Done))
			}
		}
	}

	// mtime 拨到 90 秒前（超过 60 秒窗口）→ 下一轮传过去。
	p := filepathJoin(c.sync, relA, name)
	ago := time.Now().Add(-90 * time.Second)
	if err := os.Chtimes(p, ago, ago); err != nil {
		t.Fatalf("拨 mtime 失败：%v", err)
	}
	rep = c.round(true)
	c.assertAligned(relA, rep)
	if got := c.ubFilesOf(relA)[name]; got != sha256Hex(content) {
		t.Errorf("过安静期后没传过去或内容不对：sha=%s", got)
	}
	t.Logf("S16 首轮（quiet 内）报告：%s", jsonOf(rep))
}

// ── S18 历史版本台账（sqlite + json 一对）───────────────

// S18 台账：项目里放 历史版本/档案台账.sqlite3 + 档案索引.json（真实 sqlite，宿主机 python3 造的）；
// Mac 加一条版本 → Ubuntu 收到后能打开、内容对（Ubuntu 有 python3/sqlite3 就在 Ubuntu 上验，没有就把文件取回本机验并说明）；
// Ubuntu 改 → Mac 收到，sqlite 完好、版本对得上。
func TestItestS18Ledger(t *testing.T) {
	c := newCluster(t)
	tmp := t.TempDir()
	db, js := filepath.Join(tmp, "档案台账.sqlite3"), filepath.Join(tmp, "档案索引.json")
	macLedgerWrite(t, db, js, []string{"V001"})
	db1, err := os.ReadFile(db)
	if err != nil {
		t.Fatalf("读台账 db 失败：%v", err)
	}
	if got := macSQLiteDump(t, db); !strings.Contains(got, "ok") || !strings.Contains(got, "V001") {
		t.Fatalf("本机自造的台账就不对：%s", got)
	}
	c.startUnit(relB, map[string][]byte{
		"报告.txt": []byte("报告\n"),
		ledgerDB: db1,
		ledgerJS: readOrFatal(t, js),
	})
	rep := c.round(true)
	c.assertAligned(relB, rep)
	if got := c.ubFilesOf(relB)[ledgerDB]; got != sha256Hex(db1) {
		t.Errorf("首轮 Ubuntu 台账字节不对：sha=%s", got)
	}
	if dump := c.ubSQLiteDump(context.Background(), c.env.root+"/"+relB+"/"+ledgerDB); dump != "" {
		if !strings.Contains(dump, "ok") || !strings.Contains(dump, "V001") {
			t.Errorf("首轮 Ubuntu 上打开台账不对：dump=%s", dump)
		}
	} else {
		t.Logf("Ubuntu 没有 python3/sqlite3：打开校验降级为本机验 + 两边 sha 一致")
	}

	// Mac 加一条版本 → Ubuntu 收到后 sqlite 能打开、内容对。
	macLedgerWrite(t, db, js, []string{"V001", "V002"})
	c.writeMacInSync(relB, map[string][]byte{ledgerDB: readOrFatal(t, db), ledgerJS: readOrFatal(t, js)})
	c.sleepQuiet()
	rep = c.round(true)
	c.assertAligned(relB, rep)
	dump := c.ubSQLiteDump(context.Background(), c.env.root+"/"+relB+"/"+ledgerDB)
	if dump != "" && (!strings.Contains(dump, "ok") || !strings.Contains(dump, "V002")) {
		t.Errorf("Ubuntu 上打开台账不对：dump=%s", dump)
	}
	if got := c.ubFilesOf(relB)[ledgerDB]; got != sha256Hex(readOrFatal(t, db)) {
		t.Errorf("Ubuntu 台账字节和 Mac 不一致：sha=%s", got)
	}

	// Ubuntu 改（加 V003）→ Mac 收到。
	if strings.HasSuffix(c.ubTool(context.Background()), "python3") {
		_, stderr, exit, err := c.env.runner.Run(context.Background(),
			"python3 -c '"+pyWriteLedger+"' "+quoteShell(c.env.root+"/"+relB+"/"+ledgerDB)+" "+
				quoteShell(c.env.root+"/"+relB+"/"+ledgerJS)+" V001 V002 V003", nil)
		if err != nil || exit != 0 {
			t.Fatalf("Ubuntu python3 改台账失败（exit=%d）：%v\n%s", exit, err, stderr)
		}
	} else {
		// Ubuntu 没 python3：本机造好 V003 的一对推过去，内容仍是真实 sqlite。
		macLedgerWrite(t, db, js, []string{"V001", "V002", "V003"})
		c.writeUb(relB, map[string][]byte{ledgerDB: readOrFatal(t, db), ledgerJS: readOrFatal(t, js)})
	}
	c.sleepQuiet()
	rep = c.round(true)
	c.assertAligned(relB, rep)
	macDB := filepathJoin(c.sync, relB, ledgerDB)
	got := macSQLiteDump(t, macDB)
	if !strings.Contains(got, "ok") || !strings.Contains(got, "V003") {
		t.Errorf("Mac 收到的台账打不开或没有 V003：dump=%s", got)
	}
	t.Logf("S18 最终 Mac=%s Ubuntu=%s", jsonOf(mapKeys(c.macFilesOf(relB))), jsonOf(mapKeys(c.ubFilesOf(relB))))
}

func readOrFatal(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return b
}

// ── 开始/取消同步往返 ──────────────────────────────────

// 开始/取消同步往返：work_dir 里的普通项目目录 StartSync → 挪进 sync_dir、桌面留链接、一轮后 Ubuntu 有；
// StopSync → 目录回到 work_dir（实体不是链接）、sync_dir 里没了；再一轮 Ubuntu 不删、status 标「已停止同步」。
func TestItestStartStopSync(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	workP := filepathJoin(c.work, relB)
	writeTree(t, workP, map[string][]byte{"备忘.txt": []byte("备忘\n"), "素材/音频.wav": []byte("RIFF\n")})
	quietPast(t, workP)

	res, err := c.mob.StartSync(ctx, relB)
	if err != nil || res.Status != projsync.PFUnitMoveDone {
		t.Fatalf("StartSync 没做成：%v status=%s refusals=%s", err, res.Status, jsonOf(res.Refusals))
	}
	if got := lstatKind(t, workP); got != "link" {
		t.Errorf("开始同步后桌面 %s 应该是链接，实际 %s", workP, got)
	}
	if got := lstatKind(t, filepathJoin(c.sync, relB)); got != "dir" {
		t.Fatalf("开始同步后同步目录 %s 应该是实体目录，实际 %s", filepathJoin(c.sync, relB), got)
	}
	rep := c.round(true)
	c.assertAligned(relB, rep)

	if res, err := c.mob.StopSync(ctx, codeB); err != nil || res.Status != projsync.PFUnitMoveDone {
		t.Fatalf("StopSync 没做成：%v status=%s refusals=%s", err, res.Status, jsonOf(res.Refusals))
	}
	if got := lstatKind(t, workP); got != "dir" {
		t.Errorf("取消同步后项目该以实体回到 work_dir，实际 %s", got)
	}
	if got := lstatKind(t, filepathJoin(c.sync, relB)); got != "missing" {
		t.Errorf("取消同步后 sync_dir 里的项目应该没了，实际 %s", got)
	}
	ubSnap := c.ubFilesOf(relB)

	rep = c.round(true)
	if onlyMac, onlyUb, differ := cmpTree(ubSnap, c.ubFilesOf(relB)); len(onlyMac)+len(onlyUb)+len(differ) > 0 {
		t.Errorf("取消同步后 Ubuntu 那份不该被删/改：缺 %v 多 %v 不同 %v", onlyMac, onlyUb, differ)
	}
	txt := ubuntuCat(t, c.env.sf, c.env.meta+"/status/"+codeB+".json")
	if txt == "" {
		t.Fatalf("Ubuntu 上没有 status/%s.json（取消同步要有标记）", codeB)
	}
	var st projsync.PFRemoteStatus
	if err := json.Unmarshal([]byte(txt), &st); err != nil {
		t.Fatalf("解析 Ubuntu status 失败：%v\n%s", err, txt)
	}
	if !st.Stopped || !strings.Contains(st.Note, projsync.PFNoteStopped) {
		t.Errorf("Ubuntu status 没标已停止同步：%s", txt)
	}
	t.Logf("取消后 Ubuntu status：%s", txt)
}
