package wgtund

// remote.go：projsync.RemoteRunner 的 SSH 实现——复用连接池里主机的共用 SSH 连接（和 v1 项目同步用的同一条），
// 在对方机器上执行一条命令并把请求经 stdin 送入。不新建凭证、不改 hosts.json 格式。
// 不接入调度（那是后续项）。

import (
	"context"
	"time"
)

// remoteRunTimeout 是单次远程协议调用的上限（采集里每个工作区的 agent 判定最多几秒）。
const remoteRunTimeout = 2 * time.Minute

// sshRemoteRunner 实现 projsync.RemoteRunner。
type sshRemoteRunner struct {
	pool *sshPool
	host string
}

func newSSHRemoteRunner(p *sshPool, host string) *sshRemoteRunner {
	return &sshRemoteRunner{pool: p, host: host}
}

// Run 在主机上执行 cmd（命令串由调用方保证只含固定可执行路径与子命令名），stdin 原样送入。
// 连接已断时丢掉这条连接（下次调用重拨），但本次不重试：apply 可能已经执行，重放不安全。
func (r *sshRemoteRunner) Run(ctx context.Context, cmd string, stdin []byte) (stdout, stderr []byte, exit int, err error) {
	c, err := r.pool.conn(r.host)
	if err != nil {
		return nil, nil, -1, err
	}
	ctx, cancel := context.WithTimeout(ctx, remoteRunTimeout)
	defer cancel()
	stdout, stderr, exit, err = c.RunInput(ctx, cmd, stdin)
	if err != nil && connBroken(c) {
		r.pool.drop(r.host, c)
	}
	return stdout, stderr, exit, err
}
