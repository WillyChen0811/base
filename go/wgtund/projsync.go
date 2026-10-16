// projsync.go：项目同步——经当前线路交换两台机器上同一个项目的已提交代码
// （git 命名空间互推，见 go/projsync）并双向补齐两边的 Claude Code 对话目录。
// 触发方式：本机 xxyi 页面点「同步」，或远端 Claude 经协助通道发起、用户在 xxyi 点允许。

package wgtund

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
	"github.com/pkg/sftp"
)

// projsyncConf 是 hosts.json 里可选的 projsync 段。
type projsyncConf struct {
	Host       string            `json:"host"`        // 必须是 hosts 里的主机，且有 identity_files
	LocalName  string            `json:"local_name"`  // 本机命名空间，默认 "mac"
	RemoteName string            `json:"remote_name"` // 对方命名空间，默认主机名
	Projects   []projsyncProject `json:"projects"`
	V2         *projsyncV2Conf   `json:"v2"` // 可选：项目全面同步 v2（见 projsync_v2.go），缺省关闭
}

// projsyncProject 是一个要同步的项目：两边的仓库目录和各自的 Claude 对话目录。
type projsyncProject struct {
	Name         string `json:"name"`          // [A-Za-z0-9._-]，1..64，不重复
	Local        string `json:"local"`         // 本机仓库，可写 ~/；必须是 git 仓库顶层
	Remote       string `json:"remote"`        // 远端仓库绝对路径，只允许 [A-Za-z0-9._/-]，以 / 开头，不含 ..
	LocalClaude  string `json:"local_claude"`  // 可选；默认 ~/.claude/projects/<ClaudeDirName(Local 绝对路径)>
	RemoteClaude string `json:"remote_claude"` // 可选；默认 .claude/projects/<ClaudeDirName(Remote)>（相对远端登录目录）
}

const (
	projsyncRunTimeout       = 30 * time.Minute // 单次同步整体上限
	defaultProjsyncLocalName = "mac"            // local_name 的缺省值
)

var (
	// errProjsyncBusy：同一项目同时只能跑一次。
	errProjsyncBusy    = errors.New("正在同步")
	errProjsyncUnknown = errors.New("未知项目")
	projNameRe         = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`) // 项目名
	projNSRe           = regexp.MustCompile(`^[a-z0-9-]+$`)           // 命名空间名（长度另检）
	projRemoteRe       = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)    // 远端仓库绝对路径
	projRelPathRe      = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)     // 远端对话目录（相对登录目录）
)

// projsyncTarget 是一个校验通过、路径已展开的项目。
type projsyncTarget struct {
	name         string
	local        string // 本机仓库顶层（绝对路径，符号链接已解开）
	remote       string // 远端仓库绝对路径
	localClaude  string // 本机对话目录（绝对路径）
	remoteClaude string // 远端对话目录（相对远端登录目录）
}

// projsyncPrep 是校验通过后的 projsync 配置。
type projsyncPrep struct {
	host       string // 主机名
	user       string // 主机用户（拼 git 的 ssh:// 地址用）
	identity   string // 主机第一个私钥（已展开）
	knownHosts string // 本机 known_hosts（已展开）
	localName  string
	remoteName string
	targets    []projsyncTarget // 配置顺序
	v2         projsyncV2Prep   // v2 同步的调度设置（没配 v2 段时为关闭的缺省值）
}

// checkProjsyncNS 校验命名空间名：只允许 [a-z0-9-]、1..32 字节（和 projsync.SyncGit 的要求一致，
// 在启动时就挡住，不等跑到一半才报错）。
func checkProjsyncNS(name, what string) error {
	if len(name) == 0 || len(name) > 32 || !projNSRe.MatchString(name) {
		return fmt.Errorf("的 %s %q 不合规：只允许 [a-z0-9-]，长度 1..32", what, name)
	}
	return nil
}

// prepareProjsync 校验 projsync 段；没配置返回 nil。错误信息以「hosts.json: projsync」开头。
func prepareProjsync(c *fileConf) (*projsyncPrep, error) {
	pc := c.Projsync
	if pc == nil {
		return nil, nil
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("hosts.json: projsync "+format, a...)
	}
	var host *hostConf
	for i := range c.Hosts {
		if c.Hosts[i].Name == pc.Host {
			host = &c.Hosts[i]
		}
	}
	if host == nil {
		return nil, bad("的 host %q 不在 hosts 里", pc.Host)
	}
	if len(host.IdentityFiles) == 0 {
		return nil, bad("的主机 %q 没有 identity_files，git 无法选私钥", host.Name)
	}
	localName := pc.LocalName
	if localName == "" {
		localName = defaultProjsyncLocalName
	}
	if err := checkProjsyncNS(localName, "local_name"); err != nil {
		return nil, bad("%v", err)
	}
	remoteName := pc.RemoteName
	if remoteName == "" {
		remoteName = host.Name
	}
	if err := checkProjsyncNS(remoteName, "remote_name"); err != nil {
		return nil, bad("%v", err)
	}
	p := &projsyncPrep{
		host: host.Name, user: host.User,
		identity: expand(host.IdentityFiles[0]), knownHosts: expand(c.KnownHosts),
		localName: localName, remoteName: remoteName,
	}
	v2, err := prepareProjsyncV2(pc.V2)
	if err != nil {
		return nil, bad("的 v2 %v", err)
	}
	p.v2 = v2
	seen := map[string]bool{}
	for i, pr := range pc.Projects {
		if !projNameRe.MatchString(pr.Name) {
			return nil, bad("的第 %d 个项目名 %q 不合规：只允许字母数字和 . _ -，长度 1..64", i+1, pr.Name)
		}
		if seen[pr.Name] {
			return nil, bad("的项目名 %q 重复", pr.Name)
		}
		seen[pr.Name] = true
		if pr.Local == "" {
			return nil, bad("的项目 %q 缺少 local", pr.Name)
		}
		local := expand(pr.Local)
		canon, err := canonPath(local)
		if err != nil {
			return nil, bad("的项目 %q 的 local %q 不存在或解析不了: %v", pr.Name, pr.Local, err)
		}
		if top, err := gitToplevel(canon); err != nil || top != canon {
			return nil, bad("的项目 %q 的 local %q 不是 git 仓库顶层", pr.Name, pr.Local)
		}
		if pr.Remote == "" || !strings.HasPrefix(pr.Remote, "/") || strings.Contains(pr.Remote, "..") || !projRemoteRe.MatchString(pr.Remote) {
			return nil, bad("的项目 %q 的 remote %q 不合规：必须以 / 开头、只允许字母数字和 . _ / -、不能含 ..", pr.Name, pr.Remote)
		}
		localClaude := pr.LocalClaude
		if localClaude == "" {
			localClaude = "~/.claude/projects/" + projsync.ClaudeDirName(canon)
		}
		remoteClaude := pr.RemoteClaude
		if remoteClaude == "" {
			remoteClaude = ".claude/projects/" + projsync.ClaudeDirName(pr.Remote)
		} else if strings.Contains(remoteClaude, "..") || !projRelPathRe.MatchString(remoteClaude) {
			return nil, bad("的项目 %q 的 remote_claude %q 不合规：只允许字母数字和 . _ / -、不能含 ..", pr.Name, pr.RemoteClaude)
		}
		p.targets = append(p.targets, projsyncTarget{
			name: pr.Name, local: canon, remote: pr.Remote,
			localClaude: expand(localClaude), remoteClaude: remoteClaude,
		})
	}
	return p, nil
}

// projsyncSvc 是项目同步的配置与连接零件（v1 的执行与 HTTP 接口已移除，由 projsyncV2Svc 复用）。
// 这里的 pool 是 wgtund 的主机连接池；fwdAddr 是启动时为本主机开的线路转发口（git 走它）。
type projsyncSvc struct {
	pool    *sshPool
	prep    *projsyncPrep
	fwdAddr string

	// gitOverride 非 nil 时代替 gitTarget（测试里把远端换成本地路径的仓库，生产恒为 nil）。
	gitOverride func(t projsyncTarget) (string, []string, error)
}

func newProjsyncSvc(p *projsyncPrep, pool *sshPool, fwdAddr string) *projsyncSvc {
	return &projsyncSvc{pool: pool, prep: p, fwdAddr: fwdAddr}
}

func (s *projsyncSvc) target(name string) (projsyncTarget, bool) {
	for _, t := range s.prep.targets {
		if t.name == name {
			return t, true
		}
	}
	return projsyncTarget{}, false
}

// Names 返回配置顺序的项目名（给协助服务做白名单）。
func (s *projsyncSvc) Names() []string {
	names := make([]string, 0, len(s.prep.targets))
	for _, t := range s.prep.targets {
		names = append(names, t.name)
	}
	return names
}

// gitTarget 拼 git 的远端地址与环境：地址用主机当前线路（端口是 22 也照写），
// git 经启动时开好的线路转发口走，严格校验主机密钥、只用主机第一个私钥。
func (s *projsyncSvc) gitTarget(t projsyncTarget) (string, []string, error) {
	rs := s.pool.routeInfo(s.prep.host)
	if rs.Active < 0 || rs.Active >= len(rs.Routes) {
		return "", nil, fmt.Errorf("拿不到主机 %q 的当前线路", s.prep.host)
	}
	url := fmt.Sprintf("ssh://%s@%s%s", s.prep.user, rs.Routes[rs.Active].Addr, t.remote)
	return url, []string{gitSSHCommandVia(s.fwdAddr, s.prep.knownHosts, s.prep.identity)}, nil
}

// syncClaude 经共用连接的 SFTP 双向补齐两边的 Claude 对话目录；SFTP 会话用完即关。
func (s *projsyncSvc) syncClaude(ctx context.Context, t projsyncTarget) (*projsync.FileResult, error) {
	c, err := s.pool.conn(s.prep.host)
	if err != nil {
		return nil, err
	}
	var scMu sync.Mutex
	var sc *sftp.Client
	// ctx 到期：只关本次专用的 sftp 会话，不丢共用连接（会连带断掉终端、消息、其它同步）。
	// 连接真失联时由 keepalive 判定并关闭，阻塞的 SFTP 调用随之返回。
	closeAll := func() {
		scMu.Lock()
		cur := sc
		scMu.Unlock()
		if cur != nil {
			cur.Close()
		}
	}
	var fr projsync.FileResult
	err = runInterruptible(ctx, interruptGrace, closeAll, func() error {
		cl, err := c.SFTP()
		if err != nil {
			return err
		}
		scMu.Lock()
		sc = cl
		scMu.Unlock()
		defer cl.Close()
		remote := &projsync.SFTPFS{Client: cl, Root: t.remoteClaude, Run: func(ctx context.Context, cmd string) ([]byte, error) {
			return s.pool.run(ctx, s.prep.host, cmd)
		}}
		local := &projsync.LocalFS{Root: t.localClaude}
		r, err := projsync.SyncDirs(ctx, local, remote)
		if err != nil {
			return err
		}
		fr = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fr, nil
}

// interruptGrace 是 ctx 到期后等阻塞调用自己返回的宽限（测试里调小）。
var interruptGrace = 2 * time.Second

// runInterruptible 在 goroutine 里跑 work；ctx 先到期就调用 closeFn（只应关本次专用的资源，不要关共用连接；连接失联由 keepalive 兜底），
// 最多再等 grace，仍不返回就放弃等待，返回带「超时」字样的错误。work 若在等待内正常返回，则返回它的结果。
// 调用方只能在 nil 返回之后读 work 写的结果变量。
func runInterruptible(ctx context.Context, grace time.Duration, closeFn func(), work func() error) error {
	type result struct {
		err error
		p   any
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if p := recover(); p != nil {
				r.p = p
			}
			done <- r
		}()
		r.err = work()
	}()
	// work 的 panic 在调用方协程里原样重新抛出，保持与同步调用一致的行为（调用方可能有 recover）。
	get := func(r result) error {
		if r.p != nil {
			panic(r.p)
		}
		return r.err
	}
	select {
	case r := <-done:
		return get(r)
	case <-ctx.Done():
	}
	go closeFn() // 关闭本身也可能因连接失联而阻塞，不能占着调用方
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case r := <-done:
		if err := get(r); err != nil {
			return fmt.Errorf("超时：%w", ctx.Err())
		}
		return nil
	case <-t.C:
		return fmt.Errorf("超时：操作无响应，已放弃等待: %w", ctx.Err())
	}
}
