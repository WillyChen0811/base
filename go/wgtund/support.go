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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/WillyChen0811/base/go/support"
	"github.com/WillyChen0811/base/go/tmux"
)

// supportConf 是 hosts.json 里可选的 support 段。
type supportConf struct {
	Host           string                 `json:"host"`
	ClaudeBin      string                 `json:"claude_bin"`
	Workdir        string                 `json:"workdir"`
	ClaudeSettings string                 `json:"claude_settings"`
	ClaudeTools    string                 `json:"claude_tools"`
	ClaudeArgs     []string               `json:"claude_args"`
	Repos          map[string]supportRepo `json:"repos"`
	GitTunnel      string                 `json:"git_tunnel"`
}

type supportRepo struct {
	Dir    string `json:"dir"`
	Remote string `json:"remote"`
}

// claude_args 白名单：只有这些带值的参数可由配置追加。
// 其余（权限、工具、目录、插件、worktree、位置参数等）一律不许：权限与工具由 wgtund 固定给出。
var allowedClaudeArgs = []string{
	"--model", "--fallback-model", "--effort", "--max-budget-usd", "--max-turns", "--append-system-prompt",
}

// checkClaudeArgs 校验 claude_args：每个参数必须是白名单里的，且带值（"--x 值" 或 "--x=值"，值不能以 - 开头）。
func checkClaudeArgs(args []string) error {
	list := strings.Join(allowedClaudeArgs, "、")
	for i := 0; i < len(args); i++ {
		name, val, hasEq := strings.Cut(args[i], "=")
		ok := false
		for _, w := range allowedClaudeArgs {
			ok = ok || name == w
		}
		if !ok {
			return fmt.Errorf("的 claude_args 只允许 %s（且必须带值），出现了 %q：协助要求权限由用户在 xxyi 里批准，权限、工具、目录等由 wgtund 固定给出", list, args[i])
		}
		if !hasEq {
			if i+1 >= len(args) {
				return fmt.Errorf("的 claude_args 只允许 %s（且必须带值），%s 缺值", list, name)
			}
			i++
			val = args[i]
		}
		if val == "" || strings.HasPrefix(val, "-") {
			return fmt.Errorf("的 claude_args 只允许 %s（且必须带值），%s 的值 %q 不合规（不能为空或以 - 开头）", list, name, val)
		}
	}
	return nil
}

const defaultClaudeTools = "Bash,Read,Edit,Write,Glob,Grep"

var toolsRe = regexp.MustCompile(`^[A-Za-z]+(,[A-Za-z]+)*$`)

// supportPrep 是校验通过后的 support 配置。
type supportPrep struct {
	conf     *supportConf
	workdir  string
	settings map[string]any // claude_settings 的内容
	identity string         // support.host 的第一个私钥（git_tunnel 或多线路时用）
	viaRoute bool           // support.host 有多条线路：git 经线路转发口走当前线路
	denyRead []string       // 要追加进 sandbox.filesystem.denyRead 的路径
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

// pathWithin 判断 child 是否等于 parent 或在 parent 之下（按路径分段比较，不是字符串前缀）。
func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// overlaps 任一方等于或包含另一方。
func overlaps(a, b string) bool { return pathWithin(a, b) || pathWithin(b, a) }

// gitToplevel 返回 dir 所在 git 仓库顶层目录（已解开符号链接）。
func gitToplevel(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

// localhostDomain 判断 allowedDomains 的条目（去端口、转小写后）是否指向本机。
func localhostDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if h, _, err := net.SplitHostPort(d); err == nil {
		d = h
	}
	d = strings.Trim(d, "[]")
	return d == "localhost" || strings.HasSuffix(d, ".localhost") || strings.HasPrefix(d, "127.") ||
		d == "::1" || d == "0.0.0.0"
}

// checkSandboxSettings 校验 claude_settings 里的沙盒与钩子相关项。
func checkSandboxSettings(settings map[string]any, bad func(string, ...any) error) error {
	for _, rule := range []struct {
		key  string
		want bool
	}{{"enabled", true}, {"allowUnsandboxedCommands", false}, {"failIfUnavailable", true}} {
		if v, ok := jsonPath(settings, "sandbox", rule.key); !ok || v != rule.want {
			return bad("的 claude_settings 必须设置 sandbox.%s 为 %v", rule.key, rule.want)
		}
	}
	if v, ok := jsonPath(settings, "sandbox", "autoAllowBashIfSandboxed"); !ok {
		return bad("的 claude_settings 必须显式写出 sandbox.autoAllowBashIfSandboxed（true 或 false）：" +
			"true = 沙盒内的命令不经你批准直接运行（只能写工作目录和仓库、网络受限）；false = 每条命令都弹给你批准")
	} else if _, isBool := v.(bool); !isBool {
		return bad("的 claude_settings 里 sandbox.autoAllowBashIfSandboxed 必须是 true 或 false（布尔值），实际 %#v", v)
	}
	if v, ok := jsonPath(settings, "permissions", "defaultMode"); ok && v != "default" && v != "manual" {
		return bad("的 claude_settings 里 permissions.defaultMode 只能是 \"default\" 或 \"manual\"，实际 %v", v)
	}
	// 沙盒内不许连回本机服务：否则 Claude 可能拿到主令牌自己批准
	for _, k := range [][]string{
		{"sandbox", "network", "allowLocalBinding"},
		{"sandbox", "network", "allowAllUnixSockets"},
		{"sandbox", "enableWeakerNetworkIsolation"},
		{"sandbox", "enableWeakerNestedSandbox"},
		{"sandbox", "filesystem", "disabled"},
	} {
		if v, ok := jsonPath(settings, k...); ok && v == true {
			return bad("的 claude_settings 里 %s 不许为 true：沙盒内不许连回本机服务（否则 Claude 可能拿到主令牌自己批准）或削弱沙盒", strings.Join(k, "."))
		}
	}
	if v, ok := jsonPath(settings, "sandbox", "network", "allowUnixSockets"); ok && v != nil {
		if l, isList := v.([]any); !isList || len(l) > 0 {
			return bad("的 claude_settings 里 sandbox.network.allowUnixSockets 必须为空：沙盒内不许连回本机服务（否则 Claude 可能拿到主令牌自己批准）")
		}
	}
	if v, ok := jsonPath(settings, "sandbox", "network", "allowedDomains"); ok {
		if l, isList := v.([]any); isList {
			for _, e := range l {
				if d, isStr := e.(string); isStr && localhostDomain(d) {
					return bad("的 claude_settings 里 sandbox.network.allowedDomains 不许含本机地址 %q：沙盒内不许连回本机服务（否则 Claude 可能拿到主令牌自己批准）", d)
				}
			}
		}
	}
	for _, k := range []string{"disableAllHooks", "allowManagedHooksOnly"} {
		if settings[k] == true {
			return bad("的 claude_settings 里 %s 不许为 true：协助靠 PermissionRequest 钩子把授权请求交给你", k)
		}
	}
	return nil
}

// prepareSupport 校验 support 段；不合规返回带「hosts.json: support」前缀的错误。
// dir 是 wgtund 的配置目录（Claude 不许碰）。
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
	for name, r := range sc.Repos {
		if r.Dir == "" || r.Remote == "" {
			return nil, bad("的仓库 %q 要同时写 dir 和 remote", name)
		}
	}
	if err := checkClaudeArgs(sc.ClaudeArgs); err != nil {
		return nil, bad("%v", err)
	}
	if sc.ClaudeTools != "" && !toolsRe.MatchString(sc.ClaudeTools) {
		return nil, bad("的 claude_tools %q 不合规：只能是逗号分隔的工具名（字母），如 %q", sc.ClaudeTools, defaultClaudeTools)
	}
	if sc.ClaudeSettings == "" {
		return nil, bad("缺少 claude_settings（Claude Code 设置文件）")
	}
	settingsPath := expand(sc.ClaudeSettings)
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, bad("的 claude_settings 读不了: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
		return nil, bad("的 claude_settings 必须是 JSON 对象")
	}
	if err := checkSandboxSettings(settings, bad); err != nil {
		return nil, err
	}

	// 目录隔离：Claude 能写工作目录和各仓库，这些目录若包含配置，它就能改钩子
	canonWork, err := canonPath(workdir)
	if err != nil {
		return nil, bad("的 workdir %q 无法解析: %v", sc.Workdir, err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, bad("找不到 wgtund 自身路径: %v", err)
	}
	protected := []struct{ what, raw string }{
		{"配置目录", dir},
		{"claude_settings 所在目录", filepath.Dir(settingsPath)},
		{"wgtund 所在目录", filepath.Dir(self)},
	}
	canonProt := make([]string, len(protected))
	var denyRead []string
	addDeny := func(paths ...string) {
		for _, p := range paths {
			dup := false
			for _, d := range denyRead {
				dup = dup || d == p
			}
			if !dup {
				denyRead = append(denyRead, p)
			}
		}
	}
	addDeny("~/.ssh")
	for i, pr := range protected {
		cp, err := canonPath(pr.raw)
		if err != nil {
			return nil, bad("的%s %q 无法解析: %v", pr.what, pr.raw, err)
		}
		canonProt[i] = cp
		abs, _ := filepath.Abs(pr.raw)
		addDeny(abs, cp)
	}
	writable := []struct{ what, path string }{{"workdir", canonWork}}
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
		writable = append(writable, struct{ what, path string }{"仓库 " + n + " 的 dir", cd})
	}
	for i, pr := range protected {
		for _, w := range writable {
			if overlaps(canonProt[i], w.path) {
				return nil, bad("的%s（%s）与 %s（%s）互相包含：Claude 能写工作目录和仓库，若包含配置就能改掉钩子，必须互不包含",
					pr.what, canonProt[i], w.what, w.path)
			}
		}
	}

	p := &supportPrep{conf: sc, workdir: workdir, settings: settings, denyRead: denyRead}
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

// claudeArgs 生成 Claude 的启动参数：受限模式（忽略用户/项目/本地设置，只认协助设置文件）、
// 只用指定工具和 MCP 配置、文件访问限制在工作目录与各仓库目录内，最后才是调用方的追加参数。
func claudeArgs(sc *supportConf, merged string) []string {
	tools := sc.ClaudeTools
	if tools == "" {
		tools = defaultClaudeTools
	}
	args := []string{"--restricted", "--permission-prompts", "none", "--strict-mcp-config", "--tools", tools, "--settings", merged}
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

// mergedSettings 复制设置并覆盖 hooks.PermissionRequest、强制钩子不被关掉，
// 在 sandbox.filesystem.denyRead 里追加 denyRead（去重，保留原有条目），其余内容保留。
// 只拷贝、不改 base（含 sandbox/filesystem 两层 map）。
func mergedSettings(base map[string]any, hookCmd string, denyRead []string) map[string]any {
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

	sandbox := copyMap(base["sandbox"])
	fsm := copyMap(sandbox["filesystem"])
	deny, _ := fsm["denyRead"].([]any)
	merged := append([]any{}, deny...)
	for _, d := range denyRead {
		dup := false
		for _, e := range merged {
			es, _ := e.(string)
			dup = dup || es == d
		}
		if !dup {
			merged = append(merged, d)
		}
	}
	fsm["denyRead"] = merged
	sandbox["filesystem"] = fsm
	out["sandbox"] = sandbox
	return out
}

// startSupport 建协助服务。httpAddr 是本机 HTTP 监听地址；socksAddrs 是各 SOCKS 口的实际地址。
// proj 是项目同步服务（没配置 projsync 时为 nil）：配置了就把它的项目名和同步入口交给协助服务，
// 远端 Claude 发起的同步经用户批准后同步调用 proj.Run 执行。
// 主机有多条线路时还会起线路转发口，作为第二个返回值交给调用方在退出时关闭（否则为 nil）。
// 合并设置文件在 NewService 之后（拿到钩子令牌）才写，Claude 要等用户点开始才会启动。
func startSupport(p *supportPrep, dir, knownHosts, httpAddr string, socksAddrs map[string]string, pool *sshPool, proj *projsyncSvc) (*support.Service, io.Closer, error) {
	sc := p.conf
	merged := filepath.Join(dir, "support-claude.merged.json")
	bin := expand(sc.ClaudeBin) // 由 xxyi 拉起时 PATH 可能不含 ~/.local/bin，允许写 ~/ 开头的路径
	if bin == "" {
		bin = "claude"
	}
	repos := map[string]support.Repo{}
	for n, r := range sc.Repos {
		repos[n] = support.Repo{Dir: expand(r.Dir), Remote: r.Remote}
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
	cfg := support.Config{
		Runner:  support.RunnerConfig{Bin: bin, Dir: p.workdir, Args: claudeArgs(sc, merged)},
		Repos:   repos,
		GitEnv:  gitEnv,
		LogPath: filepath.Join(dir, "support-log.jsonl"),
	}
	if proj != nil {
		cfg.ProjSyncNames = proj.Names() // 配置顺序的项目名，远端只能发起这些
		cfg.ProjSync = func(ctx context.Context, name string) (any, error) {
			return proj.Run(ctx, name) // 用户已允许：同步执行并把结果原样交回远端
		}
	}
	svc := support.NewService(cfg, &supportRemote{pool: pool, host: sc.Host})

	hookURL := "http://" + httpAddr + "/support/hook?token=" + svc.HookToken()
	cmd := tmux.Quote(self) + " support-hook " + tmux.Quote(hookURL)
	b, err := json.MarshalIndent(mergedSettings(p.settings, cmd, p.denyRead), "", "  ")
	if err == nil {
		err = os.WriteFile(merged, b, 0o600)
	}
	if err == nil {
		err = os.Chmod(merged, 0o600) // 文件已存在时 WriteFile 不改权限
	}
	if err != nil {
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
