package wgtund

import (
	"context"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
	"github.com/WillyChen0811/base/go/projsync"
	"github.com/WillyChen0811/base/go/sshterm"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// 真 SSH（sshtest 回环服务器）+ 真 sshPool：经 sshRemoteRunner 走完整的 facts / apply 往返。
// 服务端的 exec 处理里直接调用 projsync.ServeFacts/ServeApply（相当于对方机器上的 xxyi-msg 子命令），
// 并记录收到的命令串，证明请求内容只走 stdin。
func TestSSHRemoteRunnerRoundTrip(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "含 空格's \"仓库\"$(x)")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	git("add", "a.txt")
	git("commit", "-q", "-m", "c1")
	c1 := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "feat")
	os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644)
	git("add", "b.txt")
	git("commit", "-q", "-m", "c2")
	c2 := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")

	const bin = "/opt/xxyi/bin/xxyi-msg"
	var mu sync.Mutex
	var cmds []string
	clientKey, clientPriv := sshtest.NewKey()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	srv := sshtest.ServeWith(ln, clientKey.PublicKey(), sshtest.Options{ExecStdin: func(cmd string, stdin []byte) (string, string, int) {
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		var out []byte
		var err error
		switch cmd {
		case bin + " projsync-facts":
			out, err = projsync.ServeFacts(context.Background(), stdin, nil)
		case bin + " projsync-apply":
			out, err = projsync.ServeApply(context.Background(), stdin)
		default:
			return "", "未知命令\n", 127
		}
		if err != nil {
			return "", err.Error(), 2
		}
		return string(out), "", 0
	}})

	dir := t.TempDir()
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "id_test")
	os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600)
	khPath := filepath.Join(dir, "known_hosts")
	os.WriteFile(khPath, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, srv.HostKey.PublicKey())+"\n"), 0o600)
	khCb, err := knownhosts.New(khPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := newSSHPool(khCb)
	defer pool.Close()
	pool.add(hostConf{Name: "ubuntu", Addr: addr, User: "chenwy", IdentityFiles: []string{keyPath}},
		[]sshterm.Dialer{&net.Dialer{Timeout: 5 * time.Second}})

	var r projsync.RemoteRunner = newSSHRemoteRunner(pool, "ubuntu")
	ctx := context.Background()
	facts, err := projsync.RemoteFacts(ctx, r, bin, projsync.FactsRequest{Repo: repo, Side: projsync.SideUbuntu})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Branches["main"] != c1 || facts.Branches["feat"] != c2 || len(facts.Worktrees) != 1 || facts.ScopeError != "" {
		t.Fatalf("facts: %+v", facts)
	}
	ar, err := projsync.RemoteApply(ctx, r, bin, projsync.ApplyRequest{Repo: repo, Plan: projsync.ApplyPlan{
		Branch: "main", OldOID: c1, NewOID: c2, Kind: projsync.KindFastForward}})
	if err != nil || !ar.OK || git("rev-parse", "refs/heads/main") != c2 {
		t.Fatalf("快进应成功: %v %+v", err, ar)
	}
	// 非零退出码 → 协议层 error
	if _, err := projsync.RemoteApply(ctx, r, bin, projsync.ApplyRequest{Repo: repo, Plan: projsync.ApplyPlan{Branch: "main", Kind: "bogus"}}); err == nil {
		t.Fatal("坏请求应报错")
	}
	// 命令不存在 → 退出码 127 也是 error
	if _, err := projsync.RemoteFacts(ctx, r, "/nope/xxyi-msg", projsync.FactsRequest{Repo: repo, Side: projsync.SideUbuntu}); err == nil {
		t.Fatal("退出码 127 应报错")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range cmds {
		if strings.Contains(c, "仓库") || strings.Contains(c, repo) || strings.Contains(c, "$(") {
			t.Fatalf("请求内容混进了命令串: %q", c)
		}
	}
	if len(cmds) != 4 {
		t.Fatalf("命令数 %d: %q", len(cmds), cmds)
	}
}

func TestSSHRemoteRunnerTimeout(t *testing.T) {
	// 上下文取消要能打断（用 sshtest 的挂起命令）
	clientKey, clientPriv := sshtest.NewKey()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	addr := ln.Addr().String()
	block := make(chan struct{})
	defer close(block)
	srv := sshtest.ServeWith(ln, clientKey.PublicKey(), sshtest.Options{ExecStdin: func(string, []byte) (string, string, int) {
		<-block
		return "", "", 0
	}})
	dir := t.TempDir()
	pemBlock, _ := ssh.MarshalPrivateKey(clientPriv, "")
	keyPath := filepath.Join(dir, "id")
	os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600)
	khPath := filepath.Join(dir, "kh")
	os.WriteFile(khPath, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, srv.HostKey.PublicKey())+"\n"), 0o600)
	khCb, _ := knownhosts.New(khPath)
	pool := newSSHPool(khCb)
	defer pool.Close()
	pool.add(hostConf{Name: "u", Addr: addr, User: "x", IdentityFiles: []string{keyPath}}, []sshterm.Dialer{&net.Dialer{Timeout: 5 * time.Second}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := newSSHRemoteRunner(pool, "u").Run(ctx, "x projsync-facts", []byte("{}"))
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("应因超时失败: %v (%v)", err, time.Since(start))
	}
}
