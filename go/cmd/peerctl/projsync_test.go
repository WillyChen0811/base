package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WillyChen0811/base/go/projsync"
)

func psRepo(t *testing.T) (dir, c1, c2 string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(dir, 0o755)
	g := func(args ...string) string {
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	g("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644)
	g("add", "a")
	g("commit", "-q", "-m", "1")
	c1 = g("rev-parse", "HEAD")
	g("checkout", "-q", "-b", "feat")
	os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
	g("add", "b")
	g("commit", "-q", "-m", "2")
	c2 = g("rev-parse", "HEAD")
	g("checkout", "-q", "main")
	return
}

func runPS(sub, in string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run([]string{sub}, strings.NewReader(in), &out, &errb)
	return code, out.String(), errb.String()
}

func TestProjsyncSubcommands(t *testing.T) {
	// agent 判定别碰真实 tmux 服务器：本测试只验证协议，Judge 读到的是临时仓库路径，没有任何窗格。
	dir, c1, c2 := psRepo(t)
	code, out, _ := runPS("projsync-facts", `{"Version":1,"Repo":`+jsonStr(dir)+`,"Side":"ubuntu"}`)
	if code != 0 {
		t.Fatalf("facts 退出码 %d", code)
	}
	var fr projsync.FactsResponse
	if err := json.Unmarshal([]byte(out), &fr); err != nil || fr.Error != "" || fr.Branches["main"] != c1 || fr.Branches["feat"] != c2 {
		t.Fatalf("facts: %v %+v", err, fr)
	}
	if _, ok := fr.Agents[dir]; !ok {
		t.Fatalf("缺 agents: %+v", fr.Agents)
	}

	// 坏输入：非 0、stdout 无输出、不写
	for _, in := range []string{"", "{", `{"Version":2,"Repo":"/x","Side":"ubuntu"}`} {
		code, out, errs := runPS("projsync-facts", in)
		if code == 0 || out != "" || errs == "" {
			t.Errorf("facts 坏输入 %q: code=%d out=%q err=%q", in, code, out, errs)
		}
	}
	bad := `{"Version":1,"Repo":` + jsonStr(dir) + `,"Plan":{"Branch":"main","OldOID":"` + c1 + `","NewOID":"` + c2 + `","Kind":"force"}}`
	if code, out, _ := runPS("projsync-apply", bad); code == 0 || out != "" {
		t.Fatalf("未知 Kind 应失败: %d %q", code, out)
	}
	if got, _ := exec.Command("git", "-C", dir, "rev-parse", "main").Output(); strings.TrimSpace(string(got)) != c1 {
		t.Fatal("坏请求不该改引用")
	}

	good := `{"Version":1,"Repo":` + jsonStr(dir) + `,"Plan":{"Branch":"main","OldOID":"` + c1 + `","NewOID":"` + c2 + `","Kind":"fast_forward"}}`
	code, out, _ = runPS("projsync-apply", good)
	var ar projsync.ApplyResponse
	if code != 0 || json.Unmarshal([]byte(out), &ar) != nil || ar.Version != 1 {
		t.Fatalf("apply: %d %q", code, out)
	}
	got, _ := exec.Command("git", "-C", dir, "rev-parse", "main").Output()
	if ar.OK != (strings.TrimSpace(string(got)) == c2) {
		t.Fatalf("响应与引用不一致: %+v %s", ar, got)
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }
