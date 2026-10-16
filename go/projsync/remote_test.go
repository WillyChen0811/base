package projsync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// 假 judge：不碰真实 tmux/进程。
func fakeJudge(ctx context.Context, wt string) AgentVerdict {
	return AgentVerdict{Verdict: "no_agent", Notes: []string{"测试：" + wt}}
}

// localRunner 在进程内模拟对方的 xxyi-msg 子命令；记录收到的命令串和 stdin。
type localRunner struct {
	cmds   []string
	stdins [][]byte
}

func (l *localRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	l.cmds = append(l.cmds, cmd)
	l.stdins = append(l.stdins, stdin)
	f := strings.Fields(cmd)
	if len(f) != 2 {
		return nil, nil, -1, fmt.Errorf("命令串不是「路径 子命令」: %q", cmd)
	}
	var out []byte
	var err error
	switch f[1] {
	case SubcmdFacts:
		out, err = ServeFacts(ctx, stdin, fakeJudge)
	case SubcmdApply:
		out, err = ServeApply(ctx, stdin)
	default:
		return nil, nil, -1, fmt.Errorf("未知子命令 %q", f[1])
	}
	if err != nil {
		return nil, []byte(err.Error()), 2, nil
	}
	return out, nil, 0, nil
}

const testBin = "/usr/local/bin/xxyi-msg"

func TestRemoteFactsRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "worktree", "add", "-q", dir+"-wt", "feat")
	os.WriteFile(dir+"-wt/dirty.txt", []byte("x"), 0o644)

	r := &localRunner{}
	resp, err := RemoteFacts(ctx, r, testBin, FactsRequest{Repo: dir, Side: SideUbuntu})
	if err != nil {
		t.Fatal(err)
	}
	want, err := CollectLocalWorktrees(ctx, dir, nil, SideUbuntu, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.Worktrees, want) || len(want) != 2 {
		t.Fatalf("工作区不一致:\n%+v\n%+v", resp.Worktrees, want)
	}
	if resp.Branches["main"] != c1 || resp.Branches["feat"] != c2 || len(resp.Branches) != 2 {
		t.Fatalf("分支: %v", resp.Branches)
	}
	if len(resp.Agents) != 2 || resp.Agents[dir].Verdict != "no_agent" {
		t.Fatalf("agents: %+v", resp.Agents)
	}
	if resp.ScopeError != "" || resp.Repo.UbuntuBusy || resp.Repo.MacBusy {
		t.Fatalf("范围/busy: %+v", resp)
	}
	if r.cmds[0] != testBin+" projsync-facts" {
		t.Fatalf("命令串: %q", r.cmds[0])
	}

	// busy：造 MERGE_HEAD
	os.WriteFile(dir+"/.git/MERGE_HEAD", []byte(c2+"\n"), 0o644)
	resp, err = RemoteFacts(ctx, r, testBin, FactsRequest{Repo: dir, Side: SideUbuntu})
	if err != nil || !resp.Repo.UbuntuBusy || resp.Repo.UbuntuBusyReason == "" || resp.Repo.MacBusy {
		t.Fatalf("busy: %v %+v", err, resp.Repo)
	}
}

func TestRemoteFactsOutOfScopeIsNotFailure(t *testing.T) {
	dir, c1, _ := ffRepo(t)
	ag(t, dir, "branch", "a=b", c1)
	resp, err := RemoteFacts(context.Background(), &localRunner{}, testBin, FactsRequest{Repo: dir, Side: SideUbuntu})
	if err != nil {
		t.Fatalf("超出范围不该是命令失败: %v", err)
	}
	if !strings.Contains(resp.ScopeError, "超出自动同步范围") || len(resp.ScopeReasons) == 0 ||
		!strings.Contains(strings.Join(resp.ScopeReasons, "\n"), "含 =") {
		t.Fatalf("scope: %q %v", resp.ScopeError, resp.ScopeReasons)
	}
	if resp.Branches["a=b"] != c1 {
		t.Fatal("分支仍应列出")
	}
}

func TestRemoteFactsNonUTF8Branch(t *testing.T) {
	dir, c1, _ := ffRepo(t)
	ag(t, dir, "update-ref", "refs/heads/bad\xff", c1)
	_, err := RemoteFacts(context.Background(), &localRunner{}, testBin, FactsRequest{Repo: dir, Side: SideUbuntu})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("应报非 UTF-8: %v", err)
	}
}

func TestRemoteApplyFastForward(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	r := &localRunner{}
	resp, err := RemoteApply(ctx, r, testBin, ApplyRequest{Repo: dir, Plan: ApplyPlan{Branch: "main", OldOID: c1, NewOID: c2, Kind: KindFastForward}})
	if err != nil || !resp.OK || resp.ErrKind != "" {
		t.Fatalf("%v %+v", err, resp)
	}
	if rev(t, dir, "refs/heads/main") != c2 {
		t.Fatal("引用没更新")
	}
	if r.cmds[0] != testBin+" projsync-apply" {
		t.Fatalf("命令串: %q", r.cmds[0])
	}
	// 引用已变化
	resp, err = RemoteApply(ctx, r, testBin, ApplyRequest{Repo: dir, Plan: ApplyPlan{Branch: "main", OldOID: c1, NewOID: c2, Kind: KindFastForward}})
	if err != nil || resp.OK || resp.ErrKind != ErrKindRefs || resp.Error == "" {
		t.Fatalf("%v %+v", err, resp)
	}
}

func TestRemoteApplyCreateAndMerge(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	base := rev(t, dir, "HEAD")
	ag(t, dir, "checkout", "-q", "-b", "a")
	a := commitFile(t, dir, "a.txt", "a\n")
	ag(t, dir, "checkout", "-q", "-b", "b", base)
	b := commitFile(t, dir, "b.txt", "b\n")
	ag(t, dir, "checkout", "-q", "main")
	r := &localRunner{}
	resp, err := RemoteApply(ctx, r, testBin, ApplyRequest{Repo: dir, Plan: ApplyPlan{Branch: "main", Kind: KindAutoMerge, Parents: [2]string{a, b}}})
	if err != nil || !resp.OK || !oidRe.MatchString(resp.NewOID) {
		t.Fatalf("%v %+v", err, resp)
	}
	resp, err = RemoteApply(ctx, r, testBin, ApplyRequest{Repo: dir, Plan: ApplyPlan{Branch: "newb", NewOID: a, Kind: KindCreateOnOther}})
	if err != nil || !resp.OK || rev(t, dir, "refs/heads/newb") != a {
		t.Fatalf("%v %+v", err, resp)
	}
}

func TestRemoteApplyOutOfScope(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "branch", "a=b", c1)
	before := snapshot(t, dir)
	resp, err := RemoteApply(context.Background(), &localRunner{}, testBin,
		ApplyRequest{Repo: dir, Plan: ApplyPlan{Branch: "a=b", OldOID: c1, NewOID: c2, Kind: KindFastForward}})
	if err != nil || resp.OK || resp.ErrKind != ErrKindOutOfScope || !strings.Contains(resp.Error, "超出自动同步范围") {
		t.Fatalf("%v %+v", err, resp)
	}
	if snapshot(t, dir) != before {
		t.Fatal("超出范围却写了东西")
	}
}

func TestServeRejectsBadInputWithoutWriting(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	before := snapshot(t, dir)
	good := func() map[string]any {
		return map[string]any{"Version": 1, "Repo": dir, "Plan": map[string]any{
			"Branch": "main", "OldOID": c1, "NewOID": c2, "Kind": "fast_forward"}}
	}
	mod := func(f func(m, p map[string]any)) []byte {
		m := good()
		f(m, m["Plan"].(map[string]any))
		b, _ := json.Marshal(m)
		return b
	}
	cases := map[string][]byte{
		"坏 JSON":     []byte(`{"Version":1,`),
		"空输入":        nil,
		"多余内容":       append(mod(func(m, p map[string]any) {}), []byte(` {}`)...),
		"版本不符":       mod(func(m, p map[string]any) { m["Version"] = 2 }),
		"缺版本":        mod(func(m, p map[string]any) { delete(m, "Version") }),
		"缺 Repo":     mod(func(m, p map[string]any) { delete(m, "Repo") }),
		"Repo 相对":    mod(func(m, p map[string]any) { m["Repo"] = "rel/x" }),
		"未知 Kind":    mod(func(m, p map[string]any) { p["Kind"] = "force_push" }),
		"nothing":    mod(func(m, p map[string]any) { p["Kind"] = "nothing" }),
		"缺 Kind":     mod(func(m, p map[string]any) { delete(p, "Kind") }),
		"缺分支":        mod(func(m, p map[string]any) { delete(p, "Branch") }),
		"坏 OID":      mod(func(m, p map[string]any) { p["NewOID"] = "xyz" }),
		"带 Env":      mod(func(m, p map[string]any) { p["Env"] = []string{"GIT_DIR=/x"} }),
		"RepoDir 不同": mod(func(m, p map[string]any) { p["RepoDir"] = "/other" }),
		"未知字段":       mod(func(m, p map[string]any) { m["Extra"] = 1 }),
	}
	for name, in := range cases {
		out, err := ServeApply(ctx, in)
		if err == nil || out != nil {
			t.Errorf("%s: 应报协议错误, out=%s err=%v", name, out, err)
		}
		if snapshot(t, dir) != before {
			t.Fatalf("%s: 发生了写入", name)
		}
	}
	// facts 同理
	for name, in := range map[string]string{
		"坏 JSON": `{`, "版本": `{"Version":9,"Repo":"/x","Side":"ubuntu"}`,
		"缺 Side": `{"Version":1,"Repo":"/x"}`, "Side mac": `{"Version":1,"Repo":"/x","Side":"mac"}`,
		"缺 Repo": `{"Version":1,"Side":"ubuntu"}`,
	} {
		if out, err := ServeFacts(ctx, []byte(in), nil); err == nil || out != nil {
			t.Errorf("facts %s: 应报协议错误", name)
		}
	}
	// 客户端：对方退出码非 0 → error
	if _, err := RemoteApply(ctx, &localRunner{}, testBin, ApplyRequest{Repo: dir, Plan: ApplyPlan{Branch: "main", Kind: "bogus"}}); err == nil {
		t.Fatal("客户端应报错")
	}
}

type recRunner struct {
	cmds   []string
	stdins [][]byte
}

func (r *recRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	r.cmds = append(r.cmds, cmd)
	r.stdins = append(r.stdins, stdin)
	return []byte(`{"Version":1,"OK":true}`), nil, 0, nil
}

func TestRemoteCommandStringNeverContainsRequest(t *testing.T) {
	ctx := context.Background()
	nasty := []string{"/tmp/a b/'q'\"d\"/$(touch pwn)/`id`/中文;rm -rf /", "x y", "$(id)", "中文 分支", "a'b\"c"}
	for _, n := range nasty {
		r := &recRunner{}
		RemoteApply(ctx, r, testBin, ApplyRequest{Repo: n, Plan: ApplyPlan{Branch: n, Kind: KindFastForward}})
		RemoteFacts(ctx, r, testBin, FactsRequest{Repo: n, Side: SideUbuntu})
		if len(r.cmds) != 2 || r.cmds[0] != testBin+" projsync-apply" || r.cmds[1] != testBin+" projsync-facts" {
			t.Fatalf("命令串被请求内容影响: %q", r.cmds)
		}
		var got ApplyRequest
		if err := json.Unmarshal(r.stdins[0], &got); err != nil || got.Repo != n || got.Plan.Branch != n {
			t.Fatalf("请求内容应原样在 stdin 里: %s", r.stdins[0])
		}
	}
}

func TestRemoteXxyiMsgPathValidation(t *testing.T) {
	ctx := context.Background()
	r := &recRunner{}
	for _, p := range []string{"", "x; id", "a b", "$(id)", "a`b`", "a'b", "-rf", "中文", "a|b", "a\nb", "a&b"} {
		if _, err := RemoteApply(ctx, r, p, ApplyRequest{}); err == nil {
			t.Errorf("路径 %q 应被拒绝", p)
		}
		if _, err := RemoteFacts(ctx, r, p, FactsRequest{}); err == nil {
			t.Errorf("路径 %q 应被拒绝", p)
		}
	}
	if len(r.cmds) != 0 {
		t.Fatal("非法路径不该执行任何命令")
	}
	for _, p := range []string{"xxyi-msg", "~/bin/xxyi-msg", "/usr/local/bin/xxyi-msg", "./a_b-1.2"} {
		if _, err := RemoteApply(ctx, r, p, ApplyRequest{}); err != nil && strings.Contains(err.Error(), "路径不合规") {
			t.Errorf("路径 %q 应通过", p)
		}
	}
}
