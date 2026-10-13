package wgtund

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
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

// 协助要求权限由用户批准：这些参数会绕过或替换权限流程，一律不许。
var forbiddenClaudeArgs = []string{
	"--dangerously-skip-permissions",
	"--allow-dangerously-skip-permissions",
	"--permission-mode",
	"--permission-prompts",
	"--settings",
	"--permission-prompt-tool",
	// 以下由 wgtund 固定给出（受限模式、工具、可访问目录），不许调用方改动
	"--setting-sources",
	"--tools",
	"--allowedTools",
	"--allowed-tools",
	"--disallowedTools",
	"--disallowed-tools",
	"--mcp-config",
	"--strict-mcp-config",
	"--add-dir",
	"--plugin-dir",
	"--agents",
	"--restricted",
	"--safe-mode",
}

const defaultClaudeTools = "Bash,Read,Edit,Write,Glob,Grep"

var toolsRe = regexp.MustCompile(`^[A-Za-z]+(,[A-Za-z]+)*$`)

// supportPrep 是校验通过后的 support 配置。
type supportPrep struct {
	conf     *supportConf
	workdir  string
	settings map[string]any // claude_settings 的内容
	identity string         // support.host 的第一个私钥（仅 git_tunnel 用）
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

// prepareSupport 校验 support 段；不合规返回带「hosts.json: support」前缀的错误。
func prepareSupport(c *fileConf) (*supportPrep, error) {
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
	for _, a := range sc.ClaudeArgs {
		for _, f := range forbiddenClaudeArgs {
			if a == f || strings.HasPrefix(a, f+"=") {
				return nil, bad("的 claude_args 不许出现 %s：协助要求权限由用户在 xxyi 里批准，不能绕过或替换", f)
			}
		}
	}
	if sc.ClaudeTools != "" && !toolsRe.MatchString(sc.ClaudeTools) {
		return nil, bad("的 claude_tools %q 不合规：只能是逗号分隔的工具名（字母），如 %q", sc.ClaudeTools, defaultClaudeTools)
	}
	if sc.ClaudeSettings == "" {
		return nil, bad("缺少 claude_settings（Claude Code 设置文件）")
	}
	raw, err := os.ReadFile(expand(sc.ClaudeSettings))
	if err != nil {
		return nil, bad("的 claude_settings 读不了: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
		return nil, bad("的 claude_settings 必须是 JSON 对象")
	}
	for _, rule := range []struct {
		key  string
		want bool
	}{{"enabled", true}, {"allowUnsandboxedCommands", false}, {"failIfUnavailable", true}} {
		if v, ok := jsonPath(settings, "sandbox", rule.key); !ok || v != rule.want {
			return nil, bad("的 claude_settings 必须设置 sandbox.%s 为 %v", rule.key, rule.want)
		}
	}
	if v, ok := jsonPath(settings, "permissions", "defaultMode"); ok && v != "default" && v != "manual" {
		return nil, bad("的 claude_settings 里 permissions.defaultMode 只能是 \"default\" 或 \"manual\"，实际 %v", v)
	}
	p := &supportPrep{conf: sc, workdir: workdir, settings: settings}
	if sc.GitTunnel != "" {
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

// claudeArgs 生成 Claude 的启动参数：受限模式（忽略用户/项目/本地设置，只认协助设置文件）、
// 只用指定工具和 MCP 配置、文件访问限制在工作目录与各仓库目录内，最后才是调用方的追加参数。
func claudeArgs(sc *supportConf, merged string) []string {
	tools := sc.ClaudeTools
	if tools == "" {
		tools = defaultClaudeTools
	}
	args := []string{"--restricted", "--strict-mcp-config", "--tools", tools, "--settings", merged}
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

// mergedSettings 复制设置并覆盖 hooks.PermissionRequest，其余内容保留。
func mergedSettings(base map[string]any, hookCmd string) map[string]any {
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	hooks := map[string]any{}
	if h, ok := base["hooks"].(map[string]any); ok {
		for k, v := range h {
			hooks[k] = v
		}
	}
	hooks["PermissionRequest"] = []any{map[string]any{
		"matcher": "*",
		"hooks": []any{map[string]any{
			"type": "command", "command": hookCmd, "timeout": 600, "onFailure": "block",
		}},
	}}
	out["hooks"] = hooks
	return out
}

// startSupport 建协助服务。httpAddr 是本机 HTTP 监听地址；socksAddrs 是各 SOCKS 口的实际地址。
// 合并设置文件在 NewService 之后（拿到钩子令牌）才写，Claude 要等用户点开始才会启动。
func startSupport(p *supportPrep, dir, knownHosts, httpAddr string, socksAddrs map[string]string, pool *sshPool) (*support.Service, error) {
	sc := p.conf
	merged := filepath.Join(dir, "support-claude.merged.json")
	bin := sc.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	repos := map[string]support.Repo{}
	for n, r := range sc.Repos {
		repos[n] = support.Repo{Dir: expand(r.Dir), Remote: r.Remote}
	}
	var gitEnv []string
	if sc.GitTunnel != "" {
		gitEnv = []string{gitSSHCommand(socksAddrs[sc.GitTunnel], knownHosts, p.identity)}
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("support: 找不到 wgtund 自身路径: %w", err)
	}
	svc := support.NewService(support.Config{
		Runner:  support.RunnerConfig{Bin: bin, Dir: p.workdir, Args: claudeArgs(sc, merged)},
		Repos:   repos,
		GitEnv:  gitEnv,
		LogPath: filepath.Join(dir, "support-log.jsonl"),
	}, &supportRemote{pool: pool, host: sc.Host})

	hookURL := "http://" + httpAddr + "/support/hook?token=" + svc.HookToken()
	cmd := tmux.Quote(self) + " support-hook " + tmux.Quote(hookURL)
	b, err := json.MarshalIndent(mergedSettings(p.settings, cmd), "", "  ")
	if err == nil {
		err = os.WriteFile(merged, b, 0o600)
	}
	if err == nil {
		err = os.Chmod(merged, 0o600) // 文件已存在时 WriteFile 不改权限
	}
	if err != nil {
		svc.Close()
		return nil, fmt.Errorf("support: 写合并设置文件失败: %w", err)
	}
	return svc, nil
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
	if err == nil {
		return ln, nil
	}
	if !connBroken(c) {
		return nil, err
	}
	r.pool.drop(r.host, c) // 连接真断了：丢掉重拨一次
	c2, err := r.pool.conn(r.host)
	if err != nil {
		return nil, err
	}
	return c2.Listen("127.0.0.1:0")
}

func (r *supportRemote) Publish(ctx context.Context, addr, token string) error {
	js, err := json.Marshal(struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}{addr, token})
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
