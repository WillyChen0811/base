package projsync

// 项目文件同步 T8：Ubuntu 侧服务端的公共部分和 poll（技术设计 §5.2 第 2 步、§8.3、§12.1、§12.2）。
//
// xxyi-msg 的 projfiles-poll / projfiles-facts / projfiles-apply 都落到这里的 PFRemoteServer.Serve：
// 读 JSON 请求（严格解码）→ 对照 <meta>/roots.json 白名单确认根 → 干活 → 写 JSON 响应。facts 在 pfremote_facts.go，
// apply 在 pfremote_apply.go。
//
// 边界：
//   - 白名单：Root 必须写在 <meta>/roots.json 里；Meta、Root 都得是干净的绝对路径（Clean 过、路径上没有符号链接），
//     Root 与 Meta 互不包含（暂存区、回收区放在根里面会被当成项目文件同步），两者都是真实目录。任何一条不满足，整个请求拒绝，什么都不做。
//   - 单元路径不信请求里的 rel：用单元发现（PFDiscoverUnits，不跟随符号链接、认编号）重新找一遍，rel 必须是发现出来的单元、
//     编号必须对得上，才拼成绝对路径。
//   - 协调者租约：poll 续租；别的协调者持有且未过期时 poll 只回租约信息，apply 整批拒绝。租约文件读不懂时不覆盖，要人工处理。
//   - 服务端只在 Serve 内部 recover：panic 变成协议层失败（退出码非 0），Mac 当结果不确定处理。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"
)

// PFRemoteServer 是 Ubuntu 侧的协议服务端。零值可用（生产用法）；字段只给测试注入。
type PFRemoteServer struct {
	Now       func() time.Time                         // 缺省 time.Now
	Stat      PFStatFunc                               // 缺省 PFLstat（扫描、写原语共用）
	Open      func(path string) (io.ReadCloser, error) // 哈希读文件用；缺省 os.Open
	ApplyHook PFApplyHook                              // 写原语的钩子（测试模拟竞态、崩溃）
}

// PFRemoteServe 用缺省服务端处理一个子命令的请求（xxyi-msg 调这个）。
// err 非 nil = 协议层失败：stdout 不该有输出，原因写 stderr；调用方对 apply 不得假定没写过。
func PFRemoteServe(ctx context.Context, sub string, in []byte) ([]byte, error) {
	return (&PFRemoteServer{}).Serve(ctx, sub, in)
}

func (s *PFRemoteServer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *PFRemoteServer) stat() PFStatFunc {
	if s.Stat != nil {
		return s.Stat
	}
	return PFLstat
}

func (s *PFRemoteServer) scanOpts(quietSeconds int) PFScanOptions {
	return PFScanOptions{Stat: s.Stat, Filter: PFRemoteScanFilter(), Now: s.Now, QuietSeconds: quietSeconds, Open: s.Open}
}

// Serve 处理 sub 子命令的一个请求。
func (s *PFRemoteServer) Serve(ctx context.Context, sub string, in []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("内部错误: %v", r)
		}
	}()
	if len(in) > PFRemoteMaxInput {
		return nil, fmt.Errorf("请求超过 %d 字节上限", PFRemoteMaxInput)
	}
	switch sub {
	case PFRemoteSubPoll:
		var req PFRemotePollRequest
		if err := decodeStrict(in, &req); err != nil {
			return nil, err
		}
		if err := pfrCheckVersion(req.Version); err != nil {
			return nil, err
		}
		if err := pfrCheckPollRequest(&req); err != nil {
			return nil, err
		}
		return s.encode(s.poll(ctx, &req))
	case PFRemoteSubFacts:
		var req PFRemoteFactsRequest
		if err := decodeStrict(in, &req); err != nil {
			return nil, err
		}
		if err := pfrCheckVersion(req.Version); err != nil {
			return nil, err
		}
		if err := pfrCheckFactsRequest(&req); err != nil {
			return nil, err
		}
		return s.encode(s.facts(ctx, &req))
	case PFRemoteSubApply:
		var req PFRemoteApplyRequest
		if err := decodeStrict(in, &req); err != nil {
			return nil, err
		}
		if err := pfrCheckVersion(req.Version); err != nil {
			return nil, err
		}
		return s.encode(s.apply(ctx, &req))
	}
	return nil, fmt.Errorf("不认识的子命令 %q", sub)
}

func pfrCheckVersion(v int) error {
	if v != PFRemoteVersion {
		return fmt.Errorf("协议版本不符：请求 %d，本端 %d", v, PFRemoteVersion)
	}
	return nil
}

// encode 编码响应；超过输出上限是协议层失败（宁可报错，绝不截断后假装完整）。
func (s *PFRemoteServer) encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > pfrOutputLimit {
		return nil, fmt.Errorf("响应有 %d 字节，超过 %d 字节上限", len(b), pfrOutputLimit)
	}
	return b, nil
}

// ── 白名单与环境 ────────────────────────────────────────

// pfrEnv 是通过白名单校验的 meta 和根。
type pfrEnv struct {
	s    *PFRemoteServer
	meta string
	root string
}

func (e *pfrEnv) unitAbs(rel string) string { return filepath.Join(e.root, filepath.FromSlash(rel)) }
func (e *pfrEnv) staging() string           { return PFRemoteStagingDir(e.meta) }
func (e *pfrEnv) recycleDir() string        { return PFRemoteRecycleDir(e.meta) }

// pfrRealDir 要求 p 是干净的绝对路径、是真实目录（不是符号链接）、路径上没有符号链接。
func pfrRealDir(what, p string) error {
	if err := pfrCheckAbsDir(what, p); err != nil {
		return err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%s %s 读不了: %w", what, p, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s %s 不是真实目录（是符号链接或文件）", what, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("%s %s 解析失败: %w", what, p, err)
	}
	if real != p {
		return fmt.Errorf("%s %s 的路径上有符号链接（实际在 %s），请在配置和 roots.json 里写实际路径", what, p, real)
	}
	return nil
}

// openEnv 校验 meta、root 并对照白名单。失败 = 拒绝整个请求。
func (s *PFRemoteServer) openEnv(meta, root string) (*pfrEnv, error) {
	if err := pfrRealDir("meta", meta); err != nil {
		return nil, err
	}
	if err := pfrRealDir("根", root); err != nil {
		return nil, err
	}
	if meta == root || pfWithin(meta, root) || pfWithin(root, meta) {
		return nil, fmt.Errorf("根 %s 与 meta %s 不能互相包含（暂存区、回收区放在根里面会被当成项目文件）", root, meta)
	}
	roots, err := PFRemoteLoadRoots(meta)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, r := range roots {
		if r == root {
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("根 %s 不在白名单 %s 里，拒绝", root, pfrJoin(meta, PFRemoteRootsFile))
	}
	return &pfrEnv{s: s, meta: meta, root: root}, nil
}

// ── 名字规则接到扫描 ────────────────────────────────────

// PFRemoteScanFilter 返回单元扫描用的名字规则（T1 的 PFExclude 与 PFCheckPath 接到 PFScanFilter）：
// 排除项（§4.1）只计数，超出范围的（§4.2 第 1、2、6 条）带原因报出。Mac 侧扫描应当用同一套规则，
// 两边才会对同一个名字做出同样的判断。
func PFRemoteScanFilter() PFScanFilter {
	return func(rel string, isDir bool) PFScanVerdict {
		parent, name := "", rel
		if i := lastSlash(rel); i >= 0 {
			parent, name = rel[:i], rel[i+1:]
		}
		if _, ex := PFExclude(name, isDir, parent); ex {
			return PFScanVerdict{Kind: PFScanExclude}
		}
		if o := PFCheckPath(rel, isDir); o != nil {
			return PFScanVerdict{Kind: PFScanOutOfScope, Reason: o.Reason}
		}
		return PFScanVerdict{}
	}
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func pfrSummary(res *PFScanResult) PFRemoteSummary {
	sm := res.Summary()
	return PFRemoteSummary{Items: len(res.Items), Dirs: sm.Dirs, Files: sm.Files, Bytes: sm.Bytes, Unknown: sm.Unknown,
		UnknownNotQuiet: sm.UnknownNotQuiet, UnknownPlaceholder: sm.UnknownPlaceholder, OutOfScope: sm.OutOfScope, Excluded: sm.Excluded}
}

// dirID 取单元目录的 dev+ino。
func (s *PFRemoteServer) dirID(abs string) (PFDirID, error) {
	st, err := s.stat()(abs)
	if err != nil {
		return PFDirID{}, err
	}
	if st.Kind != PFKindDir {
		return PFDirID{}, fmt.Errorf("%s 不是真实目录", abs)
	}
	return PFDirID{Dev: st.Dev, Ino: st.Ino}, nil
}

// ── poll ────────────────────────────────────────────────

func pfrCheckPollRequest(req *PFRemotePollRequest) error {
	if !pfrCoordIDRe.MatchString(req.CoordID) {
		return fmt.Errorf("coord_id %q 不合法", req.CoordID)
	}
	if len(req.CoordHost) > 256 {
		return errors.New("coord_host 太长")
	}
	if req.LeaseSeconds != 0 && (time.Duration(req.LeaseSeconds)*time.Second < pfrMinLease || time.Duration(req.LeaseSeconds)*time.Second > pfrMaxLease) {
		return fmt.Errorf("lease_seconds %d 超出 %d–%d", req.LeaseSeconds, int(pfrMinLease.Seconds()), int(pfrMaxLease.Seconds()))
	}
	if req.QuietSeconds < 0 || req.QuietSeconds > 3600 {
		return fmt.Errorf("quiet_seconds %d 超出 0–3600", req.QuietSeconds)
	}
	return nil
}

// renewLease 续租（或在没人持有时拿租约）。别的协调者持有且未过期、或租约文件读不懂 → 不授予，不覆盖。
func (e *pfrEnv) renewLease(id, host string, ttl time.Duration) (PFRemoteLeaseResult, error) {
	now := e.s.now()
	cur, err := pfrReadLease(e.meta)
	if err != nil {
		return PFRemoteLeaseResult{Reason: "租约文件 coordinator.json 读不懂，没有覆盖，请人工处理: " + err.Error()}, nil
	}
	if cur != nil && cur.ID != id && cur.expiresAt().After(now) {
		return PFRemoteLeaseResult{Holder: *cur,
			Reason: fmt.Sprintf("协调者 %s（%s）持有租约，到 %s 过期", cur.ID, cur.Host, cur.Expires)}, nil
	}
	nl := PFRemoteLease{Version: 1, ID: id, Host: host, Expires: now.Add(ttl).UTC().Format(time.RFC3339)}
	if err := pfrWriteLease(e.meta, nl); err != nil {
		return PFRemoteLeaseResult{}, fmt.Errorf("写租约失败: %w", err)
	}
	return PFRemoteLeaseResult{Granted: true, Holder: nl}, nil
}

// leaseBlocks 报告写入会不会被别的协调者的租约挡住（apply 用）。租约文件读不懂也算挡住：不确定就停。
func (e *pfrEnv) leaseBlocks(id string) string {
	cur, err := pfrReadLease(e.meta)
	if err != nil {
		return "租约文件 coordinator.json 读不懂，无法确认谁在写，请人工处理: " + err.Error()
	}
	if cur != nil && cur.ID != id && cur.expiresAt().After(e.s.now()) {
		return fmt.Sprintf("协调者 %s（%s）持有租约，到 %s 过期，这台不该写", cur.ID, cur.Host, cur.Expires)
	}
	return ""
}

func (s *PFRemoteServer) poll(ctx context.Context, req *PFRemotePollRequest) PFRemotePollResponse {
	resp := PFRemotePollResponse{Version: PFRemoteVersion, NowNs: s.now().UnixNano(), FreeBytes: -1, TotalBytes: -1,
		Units: []PFRemotePollUnit{}, Requests: []PFRemoteRequestFile{}}
	fail := func(f string, a ...any) PFRemotePollResponse {
		resp.Error = fmt.Sprintf(f, a...)
		return resp
	}
	env, err := s.openEnv(req.Meta, req.Root)
	if err != nil {
		return fail("%v", err)
	}
	ttl := PFRemoteDefaultLease
	if req.LeaseSeconds > 0 {
		ttl = time.Duration(req.LeaseSeconds) * time.Second
	}
	lease, err := env.renewLease(req.CoordID, req.CoordHost, ttl)
	if err != nil {
		return fail("%v", err)
	}
	resp.Lease = lease
	if !lease.Granted {
		return resp // 别人在写（或租约读不懂）：只回租约信息
	}

	disc, err := PFDiscoverUnits(env.root)
	if err != nil {
		return fail("发现单元失败: %v", err)
	}
	resp.DiscoverExcluded, resp.Incomplete = disc.Excluded, disc.Incomplete
	for _, sk := range disc.Skipped {
		resp.Skipped = append(resp.Skipped, PFRemoteSkip{Rel: sk.Rel, Reason: sk.Reason, Unreadable: sk.Unreadable})
	}
	for _, u := range disc.Units {
		pu := PFRemotePollUnit{Code: u.Code, Rel: u.Rel, Dup: u.Dup, DupRels: u.DupRels, Others: u.Others}
		abs := env.unitAbs(u.Rel)
		id, derr := s.dirID(abs)
		if derr != nil {
			pu.Error = "读单元目录失败: " + derr.Error()
			resp.Units = append(resp.Units, pu)
			continue
		}
		pu.DirID = id
		if !u.Dup {
			res, serr := PFScan(ctx, abs, s.scanOpts(req.QuietSeconds))
			switch {
			case ctx.Err() != nil:
				return fail("已取消")
			case serr != nil:
				pu.Error = "扫描失败: " + serr.Error()
			default:
				pu.Fingerprint, pu.Summary = res.Fingerprint(), pfrSummary(res)
			}
		}
		resp.Units = append(resp.Units, pu)
	}

	if free, total, derr := pfrDiskSpace(env.root); derr == nil {
		resp.FreeBytes, resp.TotalBytes = free, total
	}
	good, bad, more, err := PFRemoteListRequests(env.meta, pfrMaxRequestFiles)
	if err != nil {
		return fail("%v", err)
	}
	resp.Requests, resp.BadRequests, resp.RequestsMore = good, bad, more
	if lo, lerr := PFStagingLeftovers(env.staging()); lerr == nil {
		for i, l := range lo {
			if i >= pfrLeftoverMax {
				break
			}
			resp.StagingLeftovers = append(resp.StagingLeftovers, PFRemoteLeftover{Path: path.Base(filepath.ToSlash(l.Path)), Kind: l.Kind, Size: l.Size, MtimeNs: l.MtimeNs})
		}
	}
	return resp
}

// pfrIsNotExist 是 errors.Is(err, fs.ErrNotExist) 的简写。
func pfrIsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
