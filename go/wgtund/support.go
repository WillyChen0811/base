package wgtund

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/WillyChen0811/base/go/support"
	"github.com/WillyChen0811/base/go/tmux"
	"github.com/WillyChen0811/base/go/xxyica"
)

// supportConf 是 hosts.json 里可选的 support 段。
type supportConf struct {
	Host           string                 `json:"host"`
	ClaudeBin      string                 `json:"claude_bin"`
	Workdir        string                 `json:"workdir"`
	ClaudeSettings string                 `json:"claude_settings"`
	ClaudeArgs     []string               `json:"claude_args"`
	Repos          map[string]supportRepo `json:"repos"`
	GitTunnel      string                 `json:"git_tunnel"`
	Autostart      *bool                  `json:"autostart"` // 启动后自动开协助；缺省/null 视为 true，false 关闭
	// AutostartMinutes 是自动开启的时长（分钟）：0 不限时，1..600 到时自动结束（到时不再重开）
	AutostartMinutes int `json:"autostart_minutes"`
}

type supportRepo struct {
	Dir       string `json:"dir"`
	Remote    string `json:"remote"`
	RemoteDir string `json:"remote_dir"` // Ubuntu 上的仓库目录；不写时从 remote 推导（见 remoteDirOf）
}

// autostartOn 报告是否启动后自动开协助：缺省视为开。
func (c *supportConf) autostartOn() bool { return c.Autostart == nil || *c.Autostart }

// supportPrep 是校验通过后的 support 配置。
type supportPrep struct {
	conf         *supportConf
	workdir      string
	settings     map[string]any // claude_settings 的内容（启动时读的那份）
	settingsPath string         // claude_settings 的路径（每次开启协助重新读）
	identity     string         // support.host 的第一个私钥（git_tunnel 或多线路时用）
	viaRoute     bool           // support.host 有多条线路：git 经线路转发口走当前线路
}

// jsonPath 在嵌套对象里取值。
func jsonPath(m map[string]any, keys ...string) (any, bool) {
	var cur any = m
	for _, k := range keys {
		o, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = o[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// canonPath 把路径规范化为绝对路径并解开符号链接（路径必须存在）。
func canonPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// gitToplevel 返回 dir 所在 git 仓库顶层目录（已解开符号链接）。
func gitToplevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

// loadClaudeSettings 读 claude_settings；path 为空（没配）时返回空设置。
// 只要求文件是 JSON 对象，不校验内容：沙盒、权限等一律照用户写的用。
func loadClaudeSettings(path string, bad func(string, ...any) error) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, bad("的 claude_settings 读不了: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
		return nil, bad("的 claude_settings 必须是 JSON 对象")
	}
	return settings, nil
}

// prepareSupport 校验 support 段；不合规返回带「hosts.json: support」前缀的错误。
func prepareSupport(c *fileConf, dir string) (*supportPrep, error) {
	sc := c.Support
	if sc == nil {
		return nil, nil
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("hosts.json: support "+format, a...)
	}
	var host *hostConf
	for i := range c.Hosts {
		if c.Hosts[i].Name == sc.Host {
			host = &c.Hosts[i]
		}
	}
	if host == nil {
		return nil, bad("的 host %q 不在 hosts 里", sc.Host)
	}
	if sc.Workdir == "" {
		return nil, bad("缺少 workdir（Claude 的工作目录）")
	}
	workdir := expand(sc.Workdir)
	if fi, err := os.Stat(workdir); err != nil || !fi.IsDir() {
		return nil, bad("的 workdir %q 不存在或不是目录", sc.Workdir)
	}
	if sc.AutostartMinutes < 0 || sc.AutostartMinutes > 600 {
		return nil, bad("的 autostart_minutes %d 不合法（0 不限时，或 1..600 分钟）", sc.AutostartMinutes)
	}
	for name, r := range sc.Repos {
		if r.Dir == "" || r.Remote == "" {
			return nil, bad("的仓库 %q 要同时写 dir 和 remote", name)
		}
	}
	settingsPath := ""
	if sc.ClaudeSettings != "" { // 可选：没配就只有协助钩子
		settingsPath = expand(sc.ClaudeSettings)
	}
	settings, err := loadClaudeSettings(settingsPath, bad)
	if err != nil {
		return nil, err
	}

	// 仓库：目录存在、是 git 顶层、互不重复
	canonRepos := map[string]string{}
	names := make([]string, 0, len(sc.Repos))
	for n := range sc.Repos {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rd := expand(sc.Repos[n].Dir)
		if fi, err := os.Stat(rd); err != nil || !fi.IsDir() {
			return nil, bad("的仓库 %q 的 dir %q 不存在或不是目录", n, sc.Repos[n].Dir)
		}
		cd, err := canonPath(rd)
		if err != nil {
			return nil, bad("的仓库 %q 的 dir %q 无法解析: %v", n, sc.Repos[n].Dir, err)
		}
		top, err := gitToplevel(cd)
		if err != nil || top != cd {
			return nil, bad("的仓库 %q 的 dir %q 不是 git 仓库顶层", n, sc.Repos[n].Dir)
		}
		for other, od := range canonRepos {
			if od == cd {
				return nil, bad("的仓库 %q 和 %q 指向同一个目录 %q", n, other, sc.Repos[n].Dir)
			}
		}
		canonRepos[n] = cd
	}
	p := &supportPrep{conf: sc, workdir: workdir, settings: settings, settingsPath: settingsPath}
	if len(hostRoutes(*host)) > 1 {
		if sc.GitTunnel != "" {
			return nil, bad("的 git_tunnel 不用写：主机 %q 有多条线路，git 会自动走当前线路", host.Name)
		}
		if len(host.IdentityFiles) == 0 {
			return nil, bad("的主机 %q 有多条线路，git 要用它的私钥，但没有 identity_files", host.Name)
		}
		p.viaRoute = true
		p.identity = expand(host.IdentityFiles[0])
	} else if sc.GitTunnel != "" {
		if _, ok := c.Socks[sc.GitTunnel]; !ok {
			return nil, bad("的 git_tunnel %q 在 socks 里没有对应条目", sc.GitTunnel)
		}
		if len(host.IdentityFiles) == 0 {
			return nil, bad("配了 git_tunnel，但主机 %q 没有 identity_files，git 无法选私钥", host.Name)
		}
		p.identity = expand(host.IdentityFiles[0])
	}
	return p, nil
}

// gitSSHCommand 生成 GIT_SSH_COMMAND：git 经 SOCKS 口走 SSH，严格校验主机密钥，只用指定私钥。
func gitSSHCommand(socksAddr, knownHosts, identity string) string {
	return fmt.Sprintf(
		"GIT_SSH_COMMAND=ssh -o ProxyCommand='nc -X 5 -x %s %%h %%p' -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -i %s",
		socksAddr, tmux.Quote(knownHosts), tmux.Quote(identity))
}

// gitSSHCommandVia 生成 GIT_SSH_COMMAND：git 经本机线路转发口（forwardAddr，形如 127.0.0.1:端口）走 SSH，
// 转发口会接到主机的当前线路；主机密钥仍按仓库 remote 里写的主机名严格校验，只用指定私钥。
func gitSSHCommandVia(forwardAddr, knownHosts, identity string) string {
	host, port, err := net.SplitHostPort(forwardAddr)
	if err != nil {
		host, port = forwardAddr, "0"
	}
	return fmt.Sprintf(
		"GIT_SSH_COMMAND=ssh -o ProxyCommand='nc %s %s' -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -i %s",
		host, port, tmux.Quote(knownHosts), tmux.Quote(identity))
}

// routeForwarder 在 127.0.0.1 上开一个转发口：每个进来的连接都拨到主机的当前线路，再双向拷贝。
func routeForwarder(pool *sshPool, host string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // 转发口已关闭
			}
			go func() {
				ctx, cancel := context.WithCancel(pool.ctx)
				defer cancel()
				up, err := pool.dialActive(ctx, host)
				if err != nil {
					c.Close()
					return
				}
				done := make(chan struct{}, 2)
				cp := func(dst, src net.Conn) {
					io.Copy(dst, src)
					done <- struct{}{}
				}
				go cp(up, c)
				go cp(c, up)
				<-done // 任一方向结束就两边都关
				c.Close()
				up.Close()
				<-done
			}()
		}
	}()
	return ln, nil
}

// claudeArgs 生成 Claude 的启动参数：自动模式（用户信任它在后台干活，只有分类器拿不准的才经钩子问用户）、
// 权限提示全交给钩子（--permission-prompts none）、
// 用协助合并设置文件、各仓库目录加入可访问目录，最后原样追加调用方的 claude_args（不校验）。
func claudeArgs(sc *supportConf, merged string) []string {
	args := []string{"--permission-mode", "auto", "--permission-prompts", "none", "--settings", merged}
	names := make([]string, 0, len(sc.Repos))
	for n := range sc.Repos {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		args = append(args, "--add-dir", expand(sc.Repos[n].Dir))
	}
	return append(args, sc.ClaudeArgs...)
}

// copyMap 浅拷贝一层 map；m 不是 map 时返回空 map。
func copyMap(m any) map[string]any {
	src, _ := m.(map[string]any)
	out := make(map[string]any, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}

// mergedSettings 复制设置并覆盖 hooks.PermissionRequest（失败即拒绝）、强制钩子不被关掉，
// 其余内容（含 sandbox、permissions）原样保留。只拷贝、不改 base。
func mergedSettings(base map[string]any, hookCmd string) map[string]any {
	out := copyMap(base)
	hooks := copyMap(base["hooks"])
	hooks["PermissionRequest"] = []any{map[string]any{
		"matcher": "*",
		"hooks": []any{map[string]any{
			"type": "command", "command": hookCmd, "timeout": 600, "onFailure": "block",
		}},
	}}
	out["hooks"] = hooks
	out["disableAllHooks"] = false
	delete(out, "allowManagedHooksOnly")
	return out
}

// startSupport 建协助服务。httpAddr 是本机 HTTP 监听地址；socksAddrs 是各 SOCKS 口的实际地址。
// proj 是项目同步服务（没配置 projsync 时为 nil）：配置了就把它的项目名和同步入口交给协助服务，
// 远端 Claude 发起的同步经用户批准后同步调用 proj.RunOnce 执行（走 v2，返回 RoundResult）。
// 主机有多条线路时还会起线路转发口，作为第二个返回值交给调用方在退出时关闭（否则为 nil）。
// 合并设置文件在 NewService 之后（拿到钩子令牌）才写，Claude 要等用户点开始才会启动。
func startSupport(p *supportPrep, dir, knownHosts, httpAddr string, socksAddrs map[string]string, pool *sshPool, proj *projsyncV2Svc) (*support.Service, io.Closer, error) {
	sc := p.conf
	merged := filepath.Join(dir, "support-claude.merged.json")
	bin := expand(sc.ClaudeBin) // 由 xxyi 拉起时 PATH 可能不含 ~/.local/bin，允许写 ~/ 开头的路径
	if bin == "" {
		bin = "claude"
	}
	repos := map[string]support.Repo{}
	remoteDirs := map[string]string{}
	remoteDirErrs := map[string]string{}
	for n, r := range sc.Repos {
		repos[n] = support.Repo{Dir: expand(r.Dir), Remote: r.Remote}
		if r.RemoteDir != "" {
			remoteDirs[n] = r.RemoteDir
			continue
		}
		d, err := remoteDirOf(r.Remote)
		if err != nil {
			// 推导不出 Ubuntu 目录不让 wgtund 起不来：只跳过该仓库的 Mac→Ubuntu（Sync 时报错，原因留给报错用）
			logf("support 仓库 %q：%v，Mac→Ubuntu 代码同步不可用\n", n, err)
			remoteDirErrs[n] = err.Error()
		} else {
			remoteDirs[n] = d
		}
	}
	var gitEnv []string
	var fwd net.Listener
	if p.viaRoute {
		var err error
		if fwd, err = routeForwarder(pool, sc.Host); err != nil {
			return nil, nil, fmt.Errorf("support: 开线路转发口失败: %w", err)
		}
		gitEnv = []string{gitSSHCommandVia(fwd.Addr().String(), knownHosts, p.identity)}
	} else if sc.GitTunnel != "" {
		gitEnv = []string{gitSSHCommand(socksAddrs[sc.GitTunnel], knownHosts, p.identity)}
	}
	closeFwd := func() {
		if fwd != nil {
			fwd.Close()
		}
	}
	self, err := os.Executable()
	if err != nil {
		closeFwd()
		return nil, nil, fmt.Errorf("support: 找不到 wgtund 自身路径: %w", err)
	}
	bridge := &msgBridge{
		repos:        repos,
		remoteDir:    remoteDirs,
		remoteDirErr: remoteDirErrs,
		gitEnv:       gitEnv,
		run:          msgRemoteRunner(pool, sc.Host),
		write:        msgSFTPWriter(pool, sc.Host),
	}
	cfg := support.Config{
		Runner:       support.RunnerConfig{Bin: bin, Dir: p.workdir, Args: claudeArgs(sc, merged)},
		Repos:        repos,
		GitEnv:       gitEnv,
		LogPath:      filepath.Join(dir, "support-log.jsonl"),
		MsgSync:      bridge.Sync,
		MsgDeliver:   bridge.Deliver,
		MsgCapture:   bridge.Capture,
		MsgStorePath: filepath.Join(dir, "msg-store.json"),
	}
	// 证书校验/消息签名接进消息接口：CA 准备失败不能拖累 wgtund 启动，
	// 换成永远拒绝的校验器——消息接口全 401，原因打在 stderr 给人看。
	if ca, revoked, err := prepareMsgCA(); err != nil {
		logf("消息 CA 准备失败: %v；证书校验用拒绝器，消息接口不可用\n", err)
		cfg.MsgAuth = denyAllVerifier()
	} else {
		cfg.MsgAuth = xxyica.NewVerifier(ca.Public(), revoked)
		cfg.MsgSign = ca.Sign
	}
	if proj != nil {
		cfg.ProjSyncNames = proj.v1.Names() // 配置顺序的项目名，远端只能发起这些
		cfg.ProjSync = func(ctx context.Context, name string) (any, error) {
			return proj.RunOnce(ctx, name) // 用户已允许：同步执行并把结果原样交回远端
		}
	}
	// 合并设置文件：启动时写一次；之后每次开启协助重新读 claude_settings 再写，改设置不用重启
	var hookCmd string // NewService 之后才有钩子令牌
	writeMerged := func(settings map[string]any) error {
		b, err := json.MarshalIndent(mergedSettings(settings, hookCmd), "", "  ")
		if err == nil {
			err = os.WriteFile(merged, b, 0o600)
		}
		if err == nil {
			err = os.Chmod(merged, 0o600) // 文件已存在时 WriteFile 不改权限
		}
		return err
	}
	cfg.Prepare = func() error {
		settings, err := loadClaudeSettings(p.settingsPath, func(format string, a ...any) error {
			return fmt.Errorf("hosts.json: support "+format, a...)
		})
		if err != nil {
			return err
		}
		if err := writeMerged(settings); err != nil {
			return fmt.Errorf("写合并设置文件失败: %w", err)
		}
		return nil
	}
	svc := support.NewService(cfg, &supportRemote{pool: pool, host: sc.Host})
	// 消息桥的代码同步和页面手动同步共用 Service 的同仓库锁（bridge 建在 NewService 之前，这里补上）
	bridge.lock = svc.RepoLock

	hookURL := "http://" + httpAddr + "/support/hook?token=" + svc.HookToken()
	hookCmd = tmux.Quote(self) + " support-hook " + tmux.Quote(hookURL)
	if err := writeMerged(p.settings); err != nil {
		svc.Close()
		closeFwd()
		return nil, nil, fmt.Errorf("support: 写合并设置文件失败: %w", err)
	}
	if fwd == nil {
		return svc, nil, nil
	}
	return svc, fwd, nil
}

const endpointFile = "~/.xxyi-support/endpoint.json"

// supportRemote 经连接池里 support.host 的共用连接实现 support.Remote。
type supportRemote struct {
	pool *sshPool
	host string
}

func (r *supportRemote) Open(ctx context.Context) (net.Listener, error) {
	c, err := r.pool.conn(r.host)
	if err != nil {
		return nil, err
	}
	ln, err := c.Listen("127.0.0.1:0")
	if err != nil {
		if !connBroken(c) {
			return nil, err
		}
		r.pool.drop(r.host, c) // 连接真断了：丢掉重拨一次
		c2, err := r.pool.conn(r.host)
		if err != nil {
			return nil, err
		}
		if ln, err = c2.Listen("127.0.0.1:0"); err != nil {
			return nil, err
		}
	}
	// sshd 的 GatewayPorts 若放开，监听会落在非回环地址，等于对外开放
	host, _, serr := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); serr != nil || ip == nil || !ip.IsLoopback() {
		ln.Close()
		return nil, fmt.Errorf("远端监听不在 127.0.0.1（检查 sshd 的 GatewayPorts）: %s", ln.Addr())
	}
	return ln, nil
}

func (r *supportRemote) Publish(ctx context.Context, addr, token string, until int64) error {
	js, err := json.Marshal(struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
		Until int64  `json:"until"`
	}{addr, token, until})
	if err != nil {
		return err
	}
	const dirp = "~/.xxyi-support"
	cmd := "umask 077 && mkdir -p " + dirp + " && printf '%s' " + tmux.Quote(string(js)) +
		" > " + dirp + "/endpoint.json.tmp && mv -f " + dirp + "/endpoint.json.tmp " + endpointFile
	return r.run(ctx, cmd)
}

func (r *supportRemote) Unpublish(ctx context.Context) error {
	return r.run(ctx, "rm -f "+endpointFile)
}

// run 执行远端命令，10 秒超时（pool.run 内含）。
func (r *supportRemote) run(ctx context.Context, cmd string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := r.pool.run(ctx, r.host, cmd)
	var ee *sshterm.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("远端命令失败(%d): %s", ee.Code, strings.TrimSpace(ee.Stderr))
	}
	return err
}
