package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// pfSetup 搭一个 Ubuntu 的根和 meta（路径不含符号链接），白名单里只有这个根。
func pfSetup(t *testing.T) (meta, root string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, root = filepath.Join(base, "meta"), filepath.Join(base, "root")
	for _, d := range []string{meta, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := projsync.PFRemoteWriteRoots(meta, []string{root}); err != nil {
		t.Fatal(err)
	}
	return meta, root
}

const pfUnitRel = "2026/260403J1001-测试项目"

func pfPut(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(pfUnitRel), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

func pfBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProjfilesSubcommands(t *testing.T) {
	meta, root := pfSetup(t)
	pfPut(t, root, "a.txt", "内容")

	// poll
	code, out, errs := runPS("projfiles-poll", pfBody(t, projsync.PFRemotePollRequest{Version: 1, Meta: meta, Root: root, CoordID: "mac-A"}))
	if code != 0 || !strings.HasSuffix(out, "\n") || errs != "" {
		t.Fatalf("poll: code=%d out=%q err=%q", code, out, errs)
	}
	var pr projsync.PFRemotePollResponse
	if err := json.Unmarshal([]byte(out), &pr); err != nil || pr.Error != "" || !pr.Lease.Granted || len(pr.Units) != 1 || pr.Units[0].Code != "260403J1001" {
		t.Fatalf("poll 响应: %v %+v", err, pr)
	}

	// facts
	code, out, _ = runPS("projfiles-facts", pfBody(t, projsync.PFRemoteFactsRequest{Version: 1, Meta: meta, Root: root, Code: "260403J1001", Rel: pfUnitRel}))
	var fr projsync.PFRemoteFactsResponse
	if code != 0 || json.Unmarshal([]byte(out), &fr) != nil || fr.Error != "" || !fr.Exists || len(fr.Items) != 1 || fr.Items[0].Hash == "" {
		t.Fatalf("facts: %d %q", code, out)
	}

	// apply
	code, out, _ = runPS("projfiles-apply", pfBody(t, projsync.PFRemoteApplyRequest{Version: 1, Meta: meta, Root: root, CoordID: "mac-A", Code: "260403J1001", Rel: pfUnitRel,
		Steps: []projsync.PFRemoteStep{{Op: projsync.PFRemoteOpMkdir, Path: "新目录"}}}))
	var ar projsync.PFRemoteApplyResponse
	if code != 0 || json.Unmarshal([]byte(out), &ar) != nil || ar.Error != "" || len(ar.Results) != 1 || ar.Results[0].Apply == nil || ar.Results[0].Apply.Status != projsync.PFApplyDone {
		t.Fatalf("apply: %d %q", code, out)
	}
	if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(pfUnitRel), "新目录")); err != nil || !fi.IsDir() {
		t.Fatal("apply 的 mkdir 没生效")
	}
}

func TestProjfilesBadInput(t *testing.T) {
	meta, root := pfSetup(t)
	pfPut(t, root, "a.txt", "内容")
	good := map[string]string{
		"projfiles-poll":  pfBody(t, projsync.PFRemotePollRequest{Version: 1, Meta: meta, Root: root, CoordID: "mac-A"}),
		"projfiles-facts": pfBody(t, projsync.PFRemoteFactsRequest{Version: 1, Meta: meta, Root: root, Code: "260403J1001", Rel: pfUnitRel}),
		"projfiles-apply": pfBody(t, projsync.PFRemoteApplyRequest{Version: 1, Meta: meta, Root: root, CoordID: "mac-A", Code: "260403J1001", Rel: pfUnitRel,
			Steps: []projsync.PFRemoteStep{{Op: projsync.PFRemoteOpMkdir, Path: "补空白"}}}),
	}
	for sub, body := range good {
		bads := map[string]string{
			"空":      "",
			"不是JSON": "{",
			"未知字段":   strings.Replace(body, `"version":1`, `"version":1,"bogus":true`, 1),
			"版本不对":   strings.Replace(body, `"version":1`, `"version":2`, 1),
			"多余内容":   body + "{}",
		}
		for name, in := range bads {
			code, out, errs := runPS(sub, in)
			if code != exitErr || out != "" || errs == "" {
				t.Errorf("%s / %s: 应是协议层失败（退出码 2、无 stdout、有 stderr）: code=%d out=%q err=%q", sub, name, code, out, errs)
			}
		}
		// stdin 超长：不读完、不执行
		code, out, errs := runPS(sub, strings.Repeat(" ", projsync.PFRemoteMaxInput+10))
		if code != exitErr || out != "" || !strings.Contains(errs, "上限") {
			t.Errorf("%s: stdin 超长应拒绝: code=%d out=%q err=%q", sub, code, out, errs)
		}
		// 恰好在上限内的合法 JSON（后面补空白）仍然可用
		pad := body + strings.Repeat(" ", projsync.PFRemoteMaxInput-len(body))
		if code, _, errs := runPS(sub, pad); code != 0 {
			t.Errorf("%s: 恰好上限大小的请求应通过: code=%d err=%q", sub, code, errs)
		}
	}
}

// 协议里「读得懂但不做」的情况：退出码 0，响应里 error 非空，并且什么都没写。
func TestProjfilesRefusedIsNotProtocolFailure(t *testing.T) {
	meta, root := pfSetup(t)
	other := filepath.Join(filepath.Dir(root), "别的根")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runPS("projfiles-apply", pfBody(t, projsync.PFRemoteApplyRequest{Version: 1, Meta: meta, Root: other, CoordID: "mac-A", Code: "260403J1001", Rel: pfUnitRel,
		Steps: []projsync.PFRemoteStep{{Op: projsync.PFRemoteOpUnitMkdir}}}))
	var ar projsync.PFRemoteApplyResponse
	if code != 0 || errs != "" || json.Unmarshal([]byte(out), &ar) != nil || !strings.Contains(ar.Error, "白名单") || len(ar.Results) != 0 {
		t.Fatalf("越界应整批拒绝（退出码 0）: %d %q %q", code, out, errs)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatal("白名单外的根里被写了东西")
	}
	// 步骤不合法也一样
	code, out, _ = runPS("projfiles-apply", pfBody(t, projsync.PFRemoteApplyRequest{Version: 1, Meta: meta, Root: root, CoordID: "mac-A",
		Steps: []projsync.PFRemoteStep{{Op: "format"}}}))
	ar = projsync.PFRemoteApplyResponse{}
	if code != 0 || json.Unmarshal([]byte(out), &ar) != nil || ar.Error == "" {
		t.Fatalf("不合法的步骤应整批拒绝: %d %q", code, out)
	}
}

func TestProjfilesUsageMentionsNewCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"nope"}, strings.NewReader(""), &out, &errb); code != exitErr {
		t.Fatalf("code=%d", code)
	}
	for _, s := range []string{"projfiles-poll", "projfiles-facts", "projfiles-apply"} {
		if !strings.Contains(errb.String(), s) {
			t.Errorf("用法里应有 %s: %s", s, errb.String())
		}
	}
}

// pfRunner 是跑真正的 run()（和 peerctl 的 main 同一条路径）的 RemoteRunner。
type pfRunner struct{}

func (pfRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	f := strings.Fields(cmd)
	if len(f) != 2 {
		return nil, nil, -1, fmt.Errorf("命令串不是「路径 子命令」: %q", cmd)
	}
	var out, errb bytes.Buffer
	code := run([]string{f[1]}, bytes.NewReader(stdin), &out, &errb)
	return out.Bytes(), errb.Bytes(), code, nil
}

// 端到端：Mac 侧客户端 → 假 RemoteRunner → peerctl 的 run() → 服务端，poll、facts、apply、verify_part 都走一遍。
func TestProjfilesClientEndToEnd(t *testing.T) {
	meta, root := pfSetup(t)
	pfPut(t, root, "报告/a.docx", "AAA")
	ctx := context.Background()
	c := &projsync.PFRemoteClient{Runner: pfRunner{}, RemoteCmd: "/usr/local/bin/peerctl", Meta: meta, Root: root, CoordID: "mac-A", CoordHost: "测试"}

	pr, err := c.Poll(ctx, projsync.PFRemotePollOptions{})
	if err != nil || len(pr.Units) != 1 || pr.Units[0].Fingerprint == "" {
		t.Fatalf("poll: %v %+v", err, pr)
	}
	u := projsync.PFRemoteUnit{Code: pr.Units[0].Code, Rel: pr.Units[0].Rel}
	facts, err := c.Facts(ctx, u, projsync.PFRemoteFactsOptions{})
	if err != nil || !facts.Exists || facts.Fingerprint != pr.Units[0].Fingerprint {
		t.Fatalf("facts: %v %+v", err, facts)
	}
	if it, ok := facts.Scan.Find("报告/a.docx"); !ok || it.State != projsync.PFStateOK || it.Hash == "" {
		t.Fatalf("a.docx: %+v", it)
	}

	// 上传方向：Mac 往 Ubuntu 暂存区放 part（这里用本机写文件代替 SFTP），目标端核验，再 install_new
	content := "新来的内容"
	sum := sha256Hex(content)
	stg := projsync.PFRemoteStagingDir(meta)
	if err := os.MkdirAll(stg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stg, sum+".part"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, n, err := c.TargetVerify()(ctx, filepath.Join(stg, sum+".part"))
	if err != nil || got != sum || n != int64(len(content)) {
		t.Fatalf("目标端核验: %q %d %v", got, n, err)
	}
	res, err := c.Apply(ctx, projsync.PFRemoteApplyOptions{Unit: u, Round: "r-1"}, []projsync.PFRemoteStep{
		{Op: projsync.PFRemoteOpInstallNew, Path: "报告/新.txt", SHA256: sum, Size: int64(len(content)), MtimeNs: time.Now().Add(-48 * time.Hour).UnixNano()},
		{Op: projsync.PFRemoteOpWriteStatus, MacStatus: &projsync.PFRemoteMacStatus{Version: 1, MacSeenAt: time.Now().UTC().Format(time.RFC3339), Network: projsync.PFRemoteNetGood}},
	})
	if err != nil || res.Stopped || len(res.Results) != 2 || res.Results[0].Apply.Status != projsync.PFApplyDone {
		t.Fatalf("apply: %v %+v", err, res)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(u.Rel), "报告", "新.txt"))
	if err != nil || string(b) != content {
		t.Fatalf("安装后的内容: %q %v", b, err)
	}
	if _, err := projsync.PFRemoteReadMacStatus(meta); err != nil {
		t.Fatalf("_mac.json: %v", err)
	}
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
