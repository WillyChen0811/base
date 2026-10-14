package wgtund

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/WillyChen0811/base/go/support"
	"github.com/WillyChen0811/base/go/tmux"
	"github.com/pkg/sftp"
)

// msgSyncTimeout 单次代码同步上限（经隧道 fetch/push 可能较慢）。
const msgSyncTimeout = 2 * time.Minute

// msgRunTimeout 单次远端短命令/写文件上限（tmux 注入、SFTP 等）；测试里调小。
var msgRunTimeout = 30 * time.Second

// msgBridge 把 Hub 的 Sync/Deliver 落到 SSH 和 git 上；字段都可在测试里换成假的。
type msgBridge struct {
	repos        map[string]support.Repo // Mac 本地仓库（Dir）+ 远端地址（Remote）
	remoteDir    map[string]string       // 仓库名 → Ubuntu 上的仓库目录
	remoteDirErr map[string]string       // 仓库名 → 推导 Ubuntu 目录失败的原因（报错时直接说清）
	gitEnv       []string
	lock         func(name string) *sync.Mutex                              // support.Service 的同仓库锁；服务没建好时为 nil
	run          func(ctx context.Context, cmd string) ([]byte, int, error) // 在 Ubuntu 执行，返回输出和退出码
	write        func(ctx context.Context, path string, data []byte) error  // 在 Ubuntu 写文件（相对家目录，自动建父目录，权限 0600）
}

// msgSyncErr 把 git 同步错误翻译成消息窗口里给人看的一句话；
// src 是发生阻塞的一侧主机名（改动留在它那里），dst 是收不到更新的一侧。
func msgSyncErr(src, dst, branch string, err error) error {
	switch {
	case errors.Is(err, support.ErrDirty):
		return fmt.Errorf("%s 上 %s 有未提交的改动", src, branch)
	case errors.Is(err, support.ErrNotFastForward):
		return fmt.Errorf("%s 上 %s 有%s没有的提交，不能快进", src, branch, dst)
	case errors.Is(err, support.ErrMismatch):
		return errors.New("推送后暂存引用被改动")
	case errors.Is(err, support.ErrRepoBusy):
		return fmt.Errorf("Ubuntu 上 %s 正在另一个工作区或 rebase 中，不能动", branch)
	default:
		return err
	}
}

// Sync 按消息来源决定方向：ubuntu: 来的把 Ubuntu 的分支拉到 Mac，
// mac: 来的把 Mac 的分支经暂存引用推到 Ubuntu。结果哈希不回报（Hub 只关心成败）。
func (b *msgBridge) Sync(ctx context.Context, m support.Message) error {
	repo, ok := b.repos[m.Repo]
	if !ok {
		return fmt.Errorf("未知仓库 %s", m.Repo)
	}
	ctx, cancel := context.WithTimeout(ctx, msgSyncTimeout)
	defer cancel()
	// 同仓库的同步（页面手动同步、别条消息触发的同步）串行；拿不到锁就等，超时放弃
	if b.lock != nil {
		if mu := b.lock(m.Repo); mu != nil {
			// 无缓冲交接：锁要么交到这里（由这里 Unlock），要么等到放弃信号后由取锁协程自己放回，
			// 不会出现取到了却没人放的情况。
			handoff := make(chan struct{})
			giveUp := make(chan struct{})
			go func() {
				mu.Lock()
				select {
				case handoff <- struct{}{}:
				case <-giveUp:
					mu.Unlock()
				}
			}()
			select {
			case <-handoff:
				defer mu.Unlock()
			case <-ctx.Done():
				close(giveUp)
				return ctx.Err()
			}
		}
	}
	switch {
	case strings.HasPrefix(m.From, "ubuntu:"):
		if _, err := support.Sync(ctx, repo, m.Branch, b.gitEnv); err != nil {
			return msgSyncErr("Mac", "Ubuntu", m.Branch, err)
		}
		return nil
	case strings.HasPrefix(m.From, "mac:"):
		dir, ok := b.remoteDir[m.Repo]
		if !ok {
			if why := b.remoteDirErr[m.Repo]; why != "" {
				return fmt.Errorf("仓库 %s 没有 Ubuntu 目录：%s", m.Repo, why)
			}
			return fmt.Errorf("仓库 %s 没有 Ubuntu 目录", m.Repo)
		}
		want, err := support.PushRef(ctx, repo, m.Branch, b.gitEnv)
		if err != nil {
			return msgSyncErr("Mac", "Ubuntu", m.Branch, err)
		}
		script, err := support.FFScript(dir, m.Branch, want)
		if err != nil {
			return err
		}
		out, code, err := b.run(ctx, "sh -c "+tmux.Quote(script))
		if err != nil {
			return fmt.Errorf("连不上 Ubuntu: %v", err) // 连接类错误
		}
		got, err := support.ParseFFResult(out, code)
		if err != nil {
			// 目录不对是配置问题，单独翻（msgSyncErr 拿不到 dir）
			if errors.Is(err, support.ErrNoRepoDir) {
				return fmt.Errorf("Ubuntu 上 %s 不是 git 仓库（检查 hosts.json 的 remote_dir）", dir)
			}
			return msgSyncErr("Ubuntu", "Mac", m.Branch, err)
		}
		if got != want {
			return errors.New("两边提交不一致")
		}
		return nil
	default:
		return fmt.Errorf("消息来源 %q 无法同步", m.From)
	}
}

// notClaudeMark 是注入包装命令在"窗格里不是 Claude"时打印的标记（配合退出码 9）。
const notClaudeMark = "XXYI_NOTCLAUDE"

// deliverCmd 拼注入命令：非立即送达时先查窗格前台程序，
// 不是 claude/node 就删掉正文文件、打印标记并以 9 退出（不进粘贴）；
// 然后括号粘贴正文、删正文文件，退出码取粘贴的结果。
// Now（用户点了立即送达）不查前台程序，pane 传空串。
func deliverCmd(m support.Message, paste, pane, msgPath string) string {
	q := tmux.Quote(msgPath)
	body := "(" + paste + ") < " + q + "; rc=$?; rm -f " + q + "; exit $rc"
	if m.Now {
		return body
	}
	check := "c=$(" + pane + " 2>/dev/null); case \"$c\" in claude|node) ;; *) rm -f " + q +
		"; printf '" + notClaudeMark + " %s\\n' \"$c\"; exit 9;; esac; "
	return check + body
}

// paneProgOfNotClaude 从注入命令的输出里取出标记后面的程序名；没有标记返回 false。
func paneProgOfNotClaude(out []byte) (string, bool) {
	s := string(out)
	i := strings.Index(s, notClaudeMark)
	if i < 0 {
		return "", false
	}
	return strings.TrimSpace(s[i+len(notClaudeMark):]), true
}

// Deliver 把发往 ubuntu:<会话> 的消息注入 tmux：附件放收件箱，正文（support.Display 统一格式）
// 写成临时文件，经括号粘贴喂进会话后立即删掉临时文件。
// 非立即送达时先确认窗格里跑的是 Claude（claude/node），不是就报 ErrPeerNotClaude。
// 会话不存在时 tmux 的报错原样带上。
func (b *msgBridge) Deliver(ctx context.Context, m support.Message) error {
	// 开头就套超时：写文件和执行命令共用，协助 Stop / Hub 取消时不会卡在 SFTP 上
	ctx, cancel := context.WithTimeout(ctx, msgRunTimeout)
	defer cancel()
	session := strings.TrimPrefix(m.To, "ubuntu:")
	attach := ""
	if len(m.File) > 0 {
		inbox := "xxyi-work/inbox/" + m.ID + "-" + m.FileName
		if err := b.write(ctx, inbox, m.File); err != nil {
			return fmt.Errorf("附件写到 Ubuntu 失败: %v", err)
		}
		attach = "~/" + inbox
	}
	text := support.Display(m, attach)
	msgPath := ".xxyi-support/msg/" + m.ID + ".txt"
	if err := b.write(ctx, msgPath, []byte(text)); err != nil {
		return fmt.Errorf("消息正文写到 Ubuntu 失败: %v", err)
	}
	paste, err := tmux.PasteCommand(session, "xxyi-"+m.ID)
	if err != nil {
		return err
	}
	pane := ""
	if !m.Now {
		if pane, err = tmux.PaneCommandCommand(session); err != nil {
			return err
		}
	}
	out, code, err := b.run(ctx, deliverCmd(m, paste, pane, msgPath))
	if err != nil {
		return fmt.Errorf("连不上 Ubuntu: %v", err)
	}
	if code == 9 {
		if prog, ok := paneProgOfNotClaude(out); ok {
			return fmt.Errorf("%w（%s 窗格里是 %q）", support.ErrPeerNotClaude, m.To, prog)
		}
	}
	if code != 0 {
		msg := out
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("注入 %s 失败：%s", m.To, strings.TrimSpace(string(msg)))
	}
	return nil
}

// remoteDirOf 从仓库 remote 推导 Ubuntu 上的仓库目录：
// ssh://user@host[:port]/abs/path → /abs/path；scp 式 user@host:/abs/path → /abs/path；
// 本地绝对路径原样返回（测试用）。相对路径没法交给 FFScript（它会把目录引号包住，
// ~ 和相对解析都失效），报「需要绝对路径」，让用户在 hosts.json 仓库里写 remote_dir。
func remoteDirOf(remote string) (string, error) {
	needAbs := func() (string, error) {
		return "", fmt.Errorf("remote %q 推不出 Ubuntu 目录：remote 需要绝对路径，或在 hosts.json 仓库里写 remote_dir", remote)
	}
	if remote == "" {
		return "", errors.New("仓库 remote 为空")
	}
	if strings.HasPrefix(remote, "ssh://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" || !strings.HasPrefix(u.Path, "/") || u.Path == "/" {
			return needAbs()
		}
		return u.Path, nil
	}
	if strings.HasPrefix(remote, "/") {
		return remote, nil
	}
	if hostPart, p, ok := strings.Cut(remote, ":"); ok && !strings.Contains(hostPart, "/") && strings.Contains(hostPart, "@") {
		if strings.HasPrefix(p, "/") && len(p) > 1 {
			return p, nil
		}
		return needAbs()
	}
	return needAbs()
}

// msgRCMark 是包装命令末尾回报退出码的行前缀。
const msgRCMark = "__XXYI_RC="

// wrapRC 把命令包成总是退出 0、标准错误并进标准输出、最后一行回报真实退出码的形式：
// sshterm.Run 在退出码非零时只给 stderr 不给 stdout，而 FFScript 的 ERR 行在 stdout 上。
func wrapRC(cmd string) string {
	return "sh -c " + tmux.Quote(cmd) + " 2>&1; printf '\\n" + msgRCMark + "%d\\n' $?"
}

// splitRC 从包装命令的输出里拆出原输出和退出码（去掉 wrapRC 补的那个换行）；找不到回报行返回 -1。
func splitRC(out []byte) ([]byte, int) {
	s := strings.TrimSuffix(string(out), "\n")
	i := strings.LastIndex(s, "\n"+msgRCMark)
	if i < 0 {
		return out, -1
	}
	code, err := strconv.Atoi(s[i+1+len(msgRCMark):])
	if err != nil {
		return out, -1
	}
	return []byte(s[:i]), code
}

// msgRemoteRunner 经连接池在 Ubuntu 执行命令，拿到输出和退出码（见 wrapRC）。
// err 只在连接类错误时非 nil。
func msgRemoteRunner(pool *sshPool, host string) func(ctx context.Context, cmd string) ([]byte, int, error) {
	return func(ctx context.Context, cmd string) ([]byte, int, error) {
		c, err := pool.conn(host)
		if err != nil {
			return nil, -1, err
		}
		ctx, cancel := context.WithTimeout(ctx, msgRunTimeout)
		defer cancel()
		out, err := c.Run(ctx, wrapRC(cmd))
		if err != nil {
			var ee *sshterm.ExitError
			if errors.As(err, &ee) { // 包装后不该出现，保险起见
				return []byte(ee.Stderr), ee.Code, nil
			}
			return nil, -1, err
		}
		o, code := splitRC(out)
		return o, code, nil
	}
}

// msgSFTPWriter 经共用连接的 SFTP 在 Ubuntu 写文件（path 相对远端家目录）：
// 自动建父目录，权限 0600（文件已存在时 OpenFile 的 mode 不生效，写完再 Chmod 一次），SFTP 会话用完即关。
// 全部 SFTP 操作放进 goroutine：ctx 先到就关掉会话打断在途调用、返回 ctx.Err()，goroutine 自己退出，
// 否则隧道半死时 SFTP 调用会一直阻塞，协助 Stop 永远挂住。
func msgSFTPWriter(pool *sshPool, host string) func(ctx context.Context, path string, data []byte) error {
	return func(ctx context.Context, p string, data []byte) error {
		done := make(chan error, 1)
		var mu sync.Mutex
		var sc *sftp.Client // goroutine 拿到会话后放进来，ctx 分支要关它
		var abandoned bool  // ctx 先结束时会话还没建好，由 goroutine 拿到后自己关
		go func() {
			c, err := pool.conn(host)
			if err != nil {
				done <- err
				return
			}
			s, err := c.SFTP()
			if err != nil {
				done <- err
				return
			}
			mu.Lock()
			if abandoned {
				mu.Unlock()
				s.Close()
				return
			}
			sc = s
			mu.Unlock()
			defer s.Close()
			done <- sftpWriteFile(s, p, data)
		}()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			mu.Lock()
			s := sc
			if s == nil {
				abandoned = true // 会话稍后才建好，标记后由 goroutine 自己关
			}
			mu.Unlock()
			if s != nil {
				s.Close() // 打断 goroutine 里的在途调用
			}
			return ctx.Err()
		}
	}
}

// sftpWriteFile 在已建好的 SFTP 会话里写一个文件（建父目录、写完 chmod 0600）。
func sftpWriteFile(sc *sftp.Client, p string, data []byte) error {
	if i := strings.LastIndex(p, "/"); i > 0 {
		if err := sc.MkdirAll(p[:i]); err != nil {
			return err
		}
	}
	f, err := sc.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// 文件已存在时 OpenFile 不带 mode、服务端 umask 也不受控，写完显式 chmod 0600
	return sc.Chmod(p, 0o600)
}
