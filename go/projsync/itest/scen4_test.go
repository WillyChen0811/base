//go:build itest

// S20：Ubuntu 上经 xxyi 解析层 CLI 操作一个已同步的项目（两机联调）。
//
// 剧本（复核者定的）：
//  1. Mac 建项目 2026/260403J1201-测试项目戊（若干子目录/文件）→ 同步一轮到 Ubuntu；
//  2. 在 Ubuntu 上按对话服务那样跑解析层 CLI（python -m cmdproto.cli，cwd=xxyi，PYTHONPATH=base-main/py:xxyi）：
//     先 init（会话目录与库放 /home/chenwy/projfiles-itest/cli/ 下，随场景清理），
//     /open -project --code <编号> 按编号打开项目（会登记目录、设上下文），
//     /list -localfile 列项目里的全部文件 → 断言跟 Mac 一致（这条同时证明「按编号查得到项目」）；
//  3. 写操作：SFTP 在 Ubuntu 上改项目里一个文件的内容（模拟有人在 Ubuntu 改了它）→
//     /scan -version（write-local，versions.scan：对改过的文档在项目内建版本快照 + sqlite+json 台账，不连服务器）；
//     /scan -version 跑不通就把 CLI 输出原文 t.Log，退到 /add -project --parent <项目绝对路径> 建子目录当写操作（RESULT 里说明）；
//  4. 下一轮同步（projsync 双向，Ubuntu 新内容/新目录回到 Mac）→ 断言：Mac 上那个文件是新内容、
//     历史版本目录里的快照与台账（档案台账.sqlite3 + 档案索引.json）都到了 Mac 且和 Ubuntu 一致。
//
// 比较一致性时用 projsync 自己的 PFExclude 规则滤掉「不同步的内部文件」（历史版本里的 .lock/.index-*/-wal/-shm 等）——
// 它们合法地只留在写它们的那一侧，不算不一致。CLI 一律经 runner（每次新开 SSH 会话）在 Ubuntu 上跑，不在 Ubuntu 本机跑 go test。

package itest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/WillyChen0811/base/go/projsync"
)

// S20 的项目（戊）：编号必须是 PFCodeFromName 认得出的真实格式。
const (
	relE  = "2026/260403J1201-测试项目戊"
	codeE = "260403J1201"
)

// Ubuntu 上跑解析层 CLI 的现场（复核者给定；可用 PROJFILES_ITEST_* 覆盖，缺省用这里的字面量）。
// 会话目录 / 主库 sqlite 放 itestAllowedPrefix 下的 cli/ 子目录，随场景清理。
const (
	itestCLIxxyi = "/home/chenwy/Documents/codes/xxyi"
	itestCLIPy   = "/home/chenwy/miniforge3/envs/xxyi/bin/python"
	itestCLIPYPP = "/home/chenwy/Documents/codes/base-main/py:/home/chenwy/Documents/codes/xxyi"
	itestCLIDir  = "/home/chenwy/projfiles-itest/cli"
)

func itestEnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// cliRun 在 Ubuntu 上跑一条解析层 CLI，返回 stdout（原始 JSON 行）与退出码；stderr 一并 t.Log。
// 命令行是 cd <xxyi> && PYTHONPATH=<…> <python> -m cmdproto.cli <args…>，经 runner 每次新开 SSH 会话执行。
func (c *cluster) cliRun(args ...string) (string, int) {
	c.t.Helper()
	py := itestEnvOr("PROJFILES_ITEST_PYTHON", itestCLIPy)
	dir := itestEnvOr("PROJFILES_ITEST_XXYI", itestCLIxxyi)
	pyp := itestEnvOr("PROJFILES_ITEST_PYTHONPATH", itestCLIPYPP)
	cmd := "cd " + quoteShell(dir) + " && PYTHONPATH=" + quoteShell(pyp) + " " + py +
		" -m cmdproto.cli " + strings.Join(args, " ")
	stdout, stderr, exit, err := c.env.runner.Run(context.Background(), cmd, nil)
	c.t.Logf("Ubuntu CLI：%s\nexit=%d\nstdout=%s\nstderr=%s", cmd, exit, stdout, stderr)
	if err != nil {
		c.t.Fatalf("Ubuntu 跑解析层 CLI 失败（exit=%d）：%v\nstdout=%s\nstderr=%s", exit, err, stdout, stderr)
	}
	return string(stdout), exit
}

// cliOK 跑一条 CLI 并要求退出码 0、输出是 {"ok":true,...}；否则带原始输出失败（查/写阶段的硬指令用）。
func (c *cluster) cliOK(args ...string) map[string]any {
	c.t.Helper()
	out, exit := c.cliRun(args...)
	if exit != 0 {
		c.t.Fatalf("解析层指令没跑成（退出码 %d）：%s", exit, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		c.t.Fatalf("解析层输出不是合法 JSON：%v\n原文：%s", err, out)
	}
	if ok, _ := obj["ok"].(bool); !ok {
		c.t.Fatalf("解析层指令返回 ok=false：%s", out)
	}
	return obj
}

// cliResultString 取 {"ok":true,"result":{...}} 里 result 的某个字符串字段。
func cliResultString(obj map[string]any, key string) (string, bool) {
	res, _ := obj["result"].(map[string]any)
	if res == nil {
		return "", false
	}
	s, ok := res[key].(string)
	return s, ok
}

// pfKeptRel 判断一条相对路径会不会被 projsync 真的同步：路径里任何一段命中 PFExclude（不同步、只计数）就算不被保留。
// 用它把 macTree/ubuntuTree 滤成「只比该同步的东西」，历史版本里的 .lock/.index-*/-wal/-shm 等合法地只留在写入侧。
func pfKeptRel(rel string) bool {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		isDir := i < len(segs)-1
		if _, ex := projsync.PFExclude(s, isDir, strings.Join(segs[:i], "/")); ex {
			return false
		}
	}
	return true
}

// pfFilterTree 滤掉不会被同步的路径，只留下参与同步的文件清单。
func pfFilterTree(t map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range t {
		if pfKeptRel(k) {
			out[k] = v
		}
	}
	return out
}

func TestItestS20UbuntuCLI(t *testing.T) {
	c := newCluster(t)

	// 会话/库目录（在联调前缀下）——场景开始前清干净，结束也清（路径合规才动手）。
	sess := itestCLIDir + "/sess"
	db := itestCLIDir + "/main.db"
	if err := itestSafePath(itestCLIDir); err != nil {
		t.Fatalf("CLI 工作目录不合规：%v", err)
	}
	cleanCLI := func() {
		c.env.sh(context.Background(), "rm -rf -- "+quoteShell(itestCLIDir))
	}
	cleanCLI()
	t.Cleanup(cleanCLI)

	// 项目里放几种文档（解析层 /scan -version 按扩展名收文档进台账：.docx/.md/.txt/... 都在 DOCUMENT_EXTENSIONS 里）。
	docFile := "现场记录.md"
	c.startUnit(relE, map[string][]byte{
		"需求说明书.docx":  []byte("需求 v1\n"),
		docFile:        []byte("现场记录 v1（Mac 建）\n"),
		"2-调查/气象.txt":  []byte("气象观测\n"),
		"4-报告/正文.docx": []byte("报告 v1\n"),
		"素材/图一.png":    []byte("png-stub\n"),
	})

	// 第一轮：Mac → Ubuntu。
	rep := c.round(true)
	c.assertAligned(relE, rep)
	macBefore := c.macFilesOf(relE)

	// ── 解析层：init + 按编号查项目 ──
	c.cliOK("init", "--session", quoteShell(sess), "--workspace", quoteShell(c.env.root), "--db", quoteShell(db))
	c.sleepQuiet() // init 会写 filecore.db / 会话状态，过一下安静门再进下一轮（本场景这一轮不动它）

	// /open -project：按编号打开项目（登记目录、设 project.path 上下文）。
	openOut := c.cliOK("run", "--session", quoteShell(sess), "--", "/open", "-project", "--code", codeE, "--serial", "01")
	openFolder, _ := openOut["result"].(map[string]any)
	ubProjAbs, _ := cliResultString(openOut, "path") // 取不到就从 folder.path 再取
	if ubProjAbs == "" {
		if f, ok := openFolder["folder"].(map[string]any); ok {
			ubProjAbs, _ = f["path"].(string)
		}
	}
	if ubProjAbs == "" {
		t.Fatalf("/open -project 没给出项目绝对路径：%s", jsonOf(openOut))
	}
	// 查到的项目目录必须就是同步过去的那个（Ubuntu 绝对路径 == root/rel）。
	wantUb := c.env.root + "/" + relE
	if ubProjAbs != wantUb {
		t.Errorf("/open -project --code %s 打开的目录不对：%q 应为 %q", codeE, ubProjAbs, wantUb)
	}

	// /list -localfile：列项目里的全部文件 → 跟 Mac 比（先滤掉 projsync 不同步的内部文件，虽然此刻还没有）。
	listOut := c.cliOK("run", "--session", quoteShell(sess), "--max-chars", "60000", "--", "/list", "-localfile")
	cliFiles := map[string]bool{}
	if res, ok := listOut["result"].(map[string]any); ok {
		if arr, ok := res["contents"].([]any); ok {
			for _, it := range arr {
				m, _ := it.(map[string]any)
				if m == nil {
					continue
				}
				if isDir, _ := m["is_dir"].(bool); isDir {
					continue
				}
				rel, _ := m["rel_path"].(string)
				rel = strings.ReplaceAll(rel, "\\", "/")
				cliFiles[rel] = true
			}
		}
	}
	macFiles := pfFilterTree(macBefore)
	var macSet = map[string]bool{}
	for k := range macFiles {
		macSet[k] = true
	}
	// 集一致（CLI 列的文件 == Mac 同步过去的文件）。
	for k := range macSet {
		if !cliFiles[k] {
			t.Errorf("解析层没列出 Mac 上的文件 %q（CLI=%v）", k, cliFiles)
		}
	}
	for k := range cliFiles {
		if !macSet[k] {
			t.Errorf("解析层多列了 Mac 上没有的文件 %q", k)
		}
	}

	// ── 解析层写：SFTP 改一个文件内容 + /scan -version 建版本 ──
	newBody := []byte("现场记录 v2（Ubuntu 上有人改了）\n")
	c.writeUb(relE, map[string][]byte{docFile: newBody})
	c.sleepQuiet()

	usedScan := true
	scanOut, scanExit := c.cliRun("run", "--session", quoteShell(sess), "--", "/scan", "-version",
		"--project", codeE, "--serial", "01")
	scanOK := scanExit == 0
	if scanOK {
		var so map[string]any
		if json.Unmarshal([]byte(scanOut), &so) == nil {
			if ok, _ := so["ok"].(bool); !ok {
				scanOK = false
			}
			if res, ok := so["result"].(map[string]any); ok {
				if s2, ok := res["success"].(bool); ok && !s2 {
					scanOK = false
					t.Logf("/scan -version result.success=false：%s", jsonOf(res))
				}
			}
		} else {
			scanOK = false
		}
	}
	if !scanOK {
		// 退路：/add -project --parent 建子目录当写操作（RESULT 里会说明走的是退路）。
		usedScan = false
		c.t.Logf("/scan -version 没跑通（退出码 %d，输出见上一条日志），退回 /add -project --parent 建子目录当写操作", scanExit)
		_ = c.cliOK("run", "--session", quoteShell(sess), "--", "/add", "-project",
			"--code", codeE+"-x", "--parent", quoteShell(wantUb))
	}

	// ── 下一轮同步：Ubuntu 的新内容/新目录回到 Mac ──
	c.sleepQuiet()
	rep2 := c.round(true)

	// Mac 上那个文件是新内容。
	if got := c.macFilesOf(relE)[docFile]; got != sha256Hex(newBody) {
		t.Errorf("下一轮同步后 Mac 上 %s 不是 Ubuntu 改过的新内容：sha=%s 应为 %s", docFile, got, sha256Hex(newBody))
	}

	macAll := c.macFilesOf(relE)
	ubAll := c.ubFilesOf(relE)

	if usedScan {
		// 主路径：历史版本目录里的快照与台账（档案台账.sqlite3 + 档案索引.json）都到了 Mac，且和 Ubuntu 一致。
		histKeys := func(m map[string]string) []string {
			var out []string
			for k := range m {
				if strings.Contains("/"+k, "/历史版本/") &&
					(strings.HasSuffix(k, "档案台账.sqlite3") || strings.HasSuffix(k, "档案索引.json")) {
					out = append(out, k)
				}
			}
			return out
		}
		if len(histKeys(ubAll)) == 0 {
			t.Errorf("Ubuntu 上就没有历史版本台账/索引，/scan -version 没落盘：Ubuntu 清单=%s", jsonOf(mapKeys(ubAll)))
		}
		macHist := histKeys(macAll)
		if len(macHist) == 0 {
			t.Errorf("Mac 上没有历史版本的台账/索引（档案台账.sqlite3 / 档案索引.json）：Mac 清单=%s", jsonOf(mapKeys(macAll)))
		}
		// 历史版本里的台账/索引两侧都要有且内容一致。
		for _, k := range histKeys(ubAll) {
			if macAll[k] == "" {
				t.Errorf("历史版本台账/索引 %s 没同步回 Mac", k)
			} else if macAll[k] != ubAll[k] {
				t.Errorf("历史版本台账/索引 %s 两侧内容不一致：Mac=%s Ubuntu=%s", k, macAll[k], ubAll[k])
			}
		}
		t.Logf("S20 走的是 /scan -version（写本地建版本）；历史版本台账/索引=%v", macHist)
	} else {
		// 退路：/scan -version 要求项目先有档案（Report）行；裸同步的项目没有，退到 /add -project --parent
		// 在项目里建子目录当写操作。解析层建的是目录（含标准子目录，不含文件），projsync 照常带目录同步；
		// 文件清单（macFilesOf）看不到空目录，这里直接查 Mac 同步目录下这些目录是否已建出来。
		subRoot := filepathJoin(c.sync, relE, "260403J1201-x")
		if fi, err := os.Stat(subRoot); err != nil || !fi.IsDir() {
			t.Errorf("退路建的子目录没同步回 Mac：%s（err=%v）", subRoot, err)
		}
		subs, err := os.ReadDir(subRoot)
		if err != nil {
			t.Errorf("读 Mac 上的退路子目录失败：%v", err)
		}
		if len(subs) == 0 {
			t.Errorf("退路子目录 %s 里没有标准子目录（没同步过来？）", subRoot)
		}
		t.Logf("S20 走的是退路 /add -project --parent（建子目录当写操作），子目录 %s 已到 Mac（标准子目录 %d 个）", subRoot, len(subs))
	}

	// 一致性：按 projsync 同步规则过滤两侧后再比，必须完全一致（含历史版本里该同步的快照/台账、退路的子目录）。
	onlyMac, onlyUb, differ := cmpTree(pfFilterTree(macAll), pfFilterTree(ubAll))
	if len(onlyMac)+len(onlyUb)+len(differ) > 0 {
		t.Errorf("下一轮同步后项目仍不一致：Mac 独有 %v，Ubuntu 独有 %v，内容不同 %v", onlyMac, onlyUb, differ)
		c.t.Logf("Mac=%s\nUbuntu=%s\n本轮报告=%s", jsonOf(mapKeys(pfFilterTree(macAll))),
			jsonOf(mapKeys(pfFilterTree(ubAll))), jsonOf(rep2))
	}
}
