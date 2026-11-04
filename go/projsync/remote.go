package projsync

// 第 5 项（第一版第②项）：经现有 SSH 连接在对方机器（Ubuntu）上采集事实、安全执行一步更新。
//
// 对方机器上跑 `xxyi-msg projsync-facts` / `xxyi-msg projsync-apply`：stdin 读一个 JSON 请求，
// stdout 写一个 JSON 响应。退出码 0 = 协议层成功（业务失败放在响应里）；非 0 = 协议层失败（输入坏了、内部 panic 等）。
//
// 安全：请求内容（路径、分支名、OID……）只经 stdin 传输，命令串只由「固定可执行路径 + 固定子命令名」组成，
// 不拼进 shell。任何字段缺失、版本不符、JSON 不合法、未知字段，一律返回错误且不执行任何写动作。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// RemoteVersion 是协议版本号。
const RemoteVersion = 1

// MaxRemoteInput 是服务端读取请求的上限。
const MaxRemoteInput = 1 << 20

// 子命令名（固定）。
const (
	SubcmdFacts = "projsync-facts"
	SubcmdApply = "projsync-apply"
)

// ErrKind 是 ApplyResponse 里失败原因的分类。
const (
	ErrKindNone       = ""
	ErrKindOutOfScope = "out_of_scope"
	ErrKindCheckedOut = "checked_out_during_update"
	ErrKindRefs       = "refs_changed"
	ErrKindOther      = "other"
	// 自动提交（Kind=auto_commit）新增的分类。
	ErrKindNotFinished     = "not_finished"
	ErrKindNothingToCommit = "nothing_to_commit"
	ErrKindCommitFailed    = "commit_failed"
	// 提交已完成但现场需确认（ErrCommittedUncertain）。
	ErrKindCommittedUncertain = "committed_uncertain"
)

// RequestAutoCommit 是 ApplyRequest.Kind 的取值：自动提交一个工作区的未提交改动。
// Kind 为空 = 原有的分支更新动作（Plan）。协议版本仍是 1：新增的 Kind/Commit 字段用 omitempty，
// 旧动作的请求字节不变；旧服务端（不认识这两个字段）收到 auto_commit 请求会因「未知字段」整体拒绝（协议层失败、
// 未执行），所以两端需先同步升级再启用自动提交。
const RequestAutoCommit = "auto_commit"

// FactsRequest 请求采集对方仓库的事实。
type FactsRequest struct {
	Version int
	Repo    string // 绝对路径
	Side    Side   // 目前只允许 "ubuntu"
}

// AgentVerdict 是一个工作区的 agent 状态（agentstate.Judge 的结论与依据）。
type AgentVerdict struct {
	Verdict string
	Notes   []string
}

// FactsResponse 是采集结果。Error 非空表示采集失败（其余字段无意义）；超出范围不算失败，放在 Scope 里。
type FactsResponse struct {
	Version   int
	Error     string
	Worktrees []WorktreeFacts
	Repo      RepoFacts               // 只填本侧（ubuntu）的 busy
	Branches  map[string]string       // refs/heads 分支名 → OID
	Agents    map[string]AgentVerdict // 工作区路径 → agent 状态
	// Scope：对整个仓库、全部本地分支跑一次 CheckScope。ScopeError 为空表示在范围内；
	// 否则 ScopeError 是完整文本，ScopeReasons 是原因列表。
	ScopeError   string
	ScopeReasons []string
}

// ApplyRequest 请求执行一步更新。Plan.RepoDir 可空（或等于 Repo）；Plan.Env 必须为空。
//
// Kind 为空：执行 Plan（分支更新）；Kind=RequestAutoCommit：执行 Commit（Plan 必须是零值，Commit 必填）。
type ApplyRequest struct {
	Version int
	Repo    string
	Plan    ApplyPlan
	Kind    string      `json:",omitempty"`
	Commit  *CommitPlan `json:",omitempty"`
}

// ApplyResponse 是执行结果。业务失败 OK=false 并带 ErrKind/Error；协议层错误不走这里。
type ApplyResponse struct {
	Version int
	OK      bool
	NewOID  string // AutoMerge 成功时的合并提交；自动提交成功时的新提交
	ErrKind string
	Error   string
}

// AgentJudge 判定一个工作区里 agent 的状态（由 agentstate.JudgeDetail 实现）。
// agentstate 包依赖本包，所以由调用方（xxyi-msg 子命令）注入，避免循环引用。nil 时一律报 unknown。
type AgentJudge func(ctx context.Context, worktree string) AgentVerdict

func decodeStrict(in []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求 JSON 不合法: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("请求 JSON 后面还有多余内容")
	}
	return nil
}

func checkRepoPath(repo string) error {
	if repo == "" {
		return errors.New("请求缺少 Repo")
	}
	if !filepath.IsAbs(repo) || strings.ContainsRune(repo, 0) || !utf8.ValidString(repo) {
		return fmt.Errorf("Repo 必须是合法的绝对路径: %q", repo)
	}
	return nil
}

// ServeFacts 处理一个 FactsRequest（原始 JSON），返回响应 JSON。err 非 nil 表示协议层失败。
func ServeFacts(ctx context.Context, in []byte, judge AgentJudge) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("内部错误: %v", r)
		}
	}()
	var req FactsRequest
	if err := decodeStrict(in, &req); err != nil {
		return nil, err
	}
	if req.Version != RemoteVersion {
		return nil, fmt.Errorf("协议版本不符：请求 %d，本端 %d", req.Version, RemoteVersion)
	}
	if err := checkRepoPath(req.Repo); err != nil {
		return nil, err
	}
	if req.Side != SideUbuntu {
		return nil, fmt.Errorf("Side 只允许 %q，收到 %q", SideUbuntu, req.Side)
	}
	resp := collectRemoteFacts(ctx, req, judge)
	return json.Marshal(resp)
}

func collectRemoteFacts(ctx context.Context, req FactsRequest, judge AgentJudge) FactsResponse {
	fail := func(f string, a ...any) FactsResponse {
		return FactsResponse{Version: RemoteVersion, Error: fmt.Sprintf(f, a...)}
	}
	repo := req.Repo
	wts, err := CollectLocalWorktrees(ctx, repo, nil, req.Side, nil)
	if err != nil {
		return fail("采集工作区失败: %v", err)
	}
	infos, err := listWorktrees(ctx, repo, nil)
	if err != nil {
		return fail("列出工作区失败: %v", err)
	}
	var rf RepoFacts
	for _, w := range infos {
		if w.Bare {
			continue
		}
		m, err := busyMarkerOf(ctx, w.Path, nil)
		if err != nil {
			return fail("%v", err)
		}
		if m != "" && !rf.UbuntuBusy {
			rf.UbuntuBusy, rf.UbuntuBusyReason = true, m
		}
	}
	out, err := runGit(ctx, repo, nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/")
	if err != nil {
		return fail("列出本地分支失败: %v", err)
	}
	branches := map[string]string{}
	var names []string
	for _, line := range nameLines(out) {
		oid, ref, ok := strings.Cut(line, " ")
		name := strings.TrimPrefix(ref, "refs/heads/")
		if !ok || name == ref || name == "" || !oidRe.MatchString(oid) {
			return fail("无法解析分支行 %q", line)
		}
		if !utf8.ValidString(name) {
			return fail("分支名不是合法 UTF-8（%q），超出自动同步范围", name)
		}
		branches[name] = oid
		names = append(names, name)
	}
	for _, w := range wts {
		if !utf8.ValidString(w.Path) || !utf8.ValidString(w.Branch) {
			return fail("工作区路径或分支名不是合法 UTF-8（%q），超出自动同步范围", w.Path)
		}
	}
	resp := FactsResponse{Version: RemoteVersion, Worktrees: wts, Repo: rf, Branches: branches,
		Agents: map[string]AgentVerdict{}}
	for _, w := range wts {
		av := AgentVerdict{Verdict: "unknown", Notes: []string{"未提供 agent 判定"}}
		if judge != nil {
			av = judge(ctx, w.Path)
		}
		resp.Agents[w.Path] = av
	}
	if err := CheckScope(ctx, repo, nil, names); err != nil {
		var oos *ErrOutOfScope
		if errors.As(err, &oos) {
			resp.ScopeError, resp.ScopeReasons = oos.Error(), oos.Reasons
		} else {
			return fail("范围检查失败: %v", err)
		}
	}
	return resp
}

// validateApplyRequest 在执行任何写动作前校验请求；失败即协议层错误。
func validateApplyRequest(req *ApplyRequest) error {
	if req.Version != RemoteVersion {
		return fmt.Errorf("协议版本不符：请求 %d，本端 %d", req.Version, RemoteVersion)
	}
	if err := checkRepoPath(req.Repo); err != nil {
		return err
	}
	switch req.Kind {
	case "":
		if req.Commit != nil {
			return errors.New("Kind 为空时不允许带 Commit")
		}
	case RequestAutoCommit:
		return validateCommitRequest(req)
	default:
		return fmt.Errorf("不认识的请求类型 Kind=%q", req.Kind)
	}
	p := &req.Plan
	if p.RepoDir != "" && p.RepoDir != req.Repo {
		return errors.New("Plan.RepoDir 与 Repo 不一致")
	}
	if len(p.Env) != 0 {
		return errors.New("不接受 Plan.Env（不允许远程注入 git 环境变量）")
	}
	if p.Branch == "" || !utf8.ValidString(p.Branch) {
		return errors.New("Plan.Branch 缺失或不是合法 UTF-8")
	}
	switch p.Kind {
	case KindCreateOnOther:
		if p.OldOID != "" || !oidRe.MatchString(p.NewOID) {
			return errors.New("CreateBranch 需要空 OldOID 和合法 NewOID")
		}
	case KindFastForward:
		if !oidRe.MatchString(p.OldOID) || !oidRe.MatchString(p.NewOID) {
			return errors.New("FastForward 需要合法的 OldOID 和 NewOID")
		}
	case KindAutoMerge:
		if !oidRe.MatchString(p.Parents[0]) || !oidRe.MatchString(p.Parents[1]) {
			return errors.New("AutoMerge 需要两个合法的 Parents")
		}
	case KindDeleteOnOther:
		if !oidRe.MatchString(p.OldOID) || p.NewOID != "" {
			return errors.New("DeleteBranch 需要合法的 OldOID 和空 NewOID")
		}
	default:
		return fmt.Errorf("不允许的动作类型 %q（只允许 create_on_other / fast_forward / auto_merge / delete_on_other）", p.Kind)
	}
	p.RepoDir = req.Repo
	return nil
}

// validateCommitRequest 校验 auto_commit 请求：Plan 必须是零值（尤其 Env 禁止），Commit 字段合法。
func validateCommitRequest(req *ApplyRequest) error {
	if !reflect.DeepEqual(req.Plan, ApplyPlan{}) {
		return errors.New("auto_commit 请求的 Plan 必须为空（不接受 Plan.Env 等）")
	}
	c := req.Commit
	if c == nil {
		return errors.New("auto_commit 请求缺少 Commit")
	}
	if c.RepoDir != "" && c.RepoDir != req.Repo {
		return errors.New("Commit.RepoDir 与 Repo 不一致")
	}
	c.RepoDir = req.Repo
	return ValidateCommitPlan(*c)
}

// ServeApply 等同 ServeApplyWith(ctx, in, nil)：没有 judge，auto_commit 只有 Force=true 才能执行。
func ServeApply(ctx context.Context, in []byte) ([]byte, error) { return ServeApplyWith(ctx, in, nil) }

// ServeApplyWith 处理一个 ApplyRequest（原始 JSON），返回响应 JSON；judge 用于 auto_commit（Force=false 时判定 agent 是否做完）。err 非 nil 表示协议层失败：
// 请求校验阶段的失败确实未写任何东西，但内部 panic 可能发生在写动作之后，所以调用方对「协议层失败」
// 一律不得假定未写入——必须停下并重新确认现场。
func ServeApplyWith(ctx context.Context, in []byte, judge CommitJudge) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("内部错误: %v", r)
		}
	}()
	var req ApplyRequest
	if err := decodeStrict(in, &req); err != nil {
		return nil, err
	}
	if err := validateApplyRequest(&req); err != nil {
		return nil, err
	}
	resp := ApplyResponse{Version: RemoteVersion}
	var aerr error
	switch {
	case req.Kind == RequestAutoCommit:
		resp.NewOID, aerr = AutoCommit(ctx, *req.Commit, judge)
	case req.Plan.Kind == KindCreateOnOther:
		aerr = CreateBranch(ctx, req.Plan)
	case req.Plan.Kind == KindFastForward:
		aerr = FastForwardBranch(ctx, req.Plan)
	case req.Plan.Kind == KindAutoMerge:
		resp.NewOID, aerr = AutoMergeCommit(ctx, req.Plan)
	case req.Plan.Kind == KindDeleteOnOther:
		aerr = DeleteBranch(ctx, req.Plan)
	}
	if aerr != nil {
		resp.NewOID = ""
		resp.Error, resp.ErrKind = aerr.Error(), classifyApplyErr(aerr)
	} else {
		resp.OK = true
	}
	return json.Marshal(resp)
}

func classifyApplyErr(err error) string {
	var oos *ErrOutOfScope
	var co *ErrCheckedOutDuringUpdate
	var cf *ErrCommitFailed
	switch {
	case errors.Is(err, ErrNotFinished):
		return ErrKindNotFinished
	case errors.Is(err, ErrNothingToCommit):
		return ErrKindNothingToCommit
	case errors.Is(err, ErrCommittedUncertain):
		return ErrKindCommittedUncertain
	case errors.As(err, &cf):
		return ErrKindCommitFailed
	case errors.As(err, &oos):
		return ErrKindOutOfScope
	case errors.As(err, &co):
		return ErrKindCheckedOut
	case errors.Is(err, ErrRefsChanged):
		return ErrKindRefs
	}
	return ErrKindOther
}

var knownErrKinds = map[string]bool{
	ErrKindOutOfScope: true, ErrKindCheckedOut: true, ErrKindRefs: true, ErrKindOther: true,
	ErrKindNotFinished: true, ErrKindNothingToCommit: true, ErrKindCommitFailed: true,
	ErrKindCommittedUncertain: true,
}

// validateApplyResponse 校验响应的必需字段与字段组合：
// OK=true 时 ErrKind、Error 必须为空，且 NewOID 恰在 AutoMerge / AutoCommit 成功时出现并合法；
// OK=false 时 ErrKind 必须是已知分类、Error 非空、NewOID 为空。
func validateApplyResponse(req ApplyRequest, resp ApplyResponse) error {
	if resp.OK {
		if resp.ErrKind != "" || resp.Error != "" {
			return fmt.Errorf("OK=true 却带有 ErrKind=%q / Error=%q", resp.ErrKind, resp.Error)
		}
		wantOID := req.Plan.Kind == KindAutoMerge || req.Kind == RequestAutoCommit
		if wantOID && !oidRe.MatchString(resp.NewOID) {
			return fmt.Errorf("成功响应缺少合法的 NewOID: %q", resp.NewOID)
		}
		if !wantOID && resp.NewOID != "" {
			return fmt.Errorf("该动作成功时不应带 NewOID: %q", resp.NewOID)
		}
		return nil
	}
	if !knownErrKinds[resp.ErrKind] {
		return fmt.Errorf("失败响应的 ErrKind 缺失或未知: %q", resp.ErrKind)
	}
	if resp.Error == "" {
		return errors.New("失败响应缺少 Error")
	}
	if resp.NewOID != "" {
		return fmt.Errorf("失败响应不应带 NewOID: %q", resp.NewOID)
	}
	return nil
}

// ── Mac 侧客户端 ──────────────────────────────────────

// RemoteRunner 在对方机器上执行一条命令：stdin 原样送入，返回 stdout、stderr、退出码。
// err 只表示连接/执行层失败；命令自己退出码非 0 放在 exit 里。
type RemoteRunner interface {
	Run(ctx context.Context, cmd string, stdin []byte) (stdout, stderr []byte, exit int, err error)
}

var xxyiMsgPathRe = regexp.MustCompile(`^[A-Za-z0-9._/~-]+$`)

func remoteCommand(xxyiMsgPath, sub string) (string, error) {
	if !xxyiMsgPathRe.MatchString(xxyiMsgPath) || strings.HasPrefix(xxyiMsgPath, "-") {
		return "", fmt.Errorf("xxyi-msg 路径不合规（只允许 [A-Za-z0-9._/~-]）: %q", xxyiMsgPath)
	}
	return xxyiMsgPath + " " + sub, nil
}

func remoteCall(ctx context.Context, r RemoteRunner, xxyiMsgPath, sub string, req any, resp any) error {
	cmd, err := remoteCommand(xxyiMsgPath, sub)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	stdout, stderr, exit, err := r.Run(ctx, cmd, body)
	if err != nil {
		return fmt.Errorf("远程执行 %s 失败: %w", sub, err)
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(stderr))
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		return fmt.Errorf("远程 %s 退出码 %d: %s", sub, exit, msg)
	}
	if err := json.Unmarshal(stdout, resp); err != nil {
		return fmt.Errorf("远程 %s 的响应不是合法 JSON: %w", sub, err)
	}
	return nil
}

// RemoteFacts 经 r 在对方机器上采集事实。采集失败（resp.Error 非空）返回 error；超出范围不是错误（看 Scope*）。
func RemoteFacts(ctx context.Context, r RemoteRunner, xxyiMsgPath string, req FactsRequest) (FactsResponse, error) {
	req.Version = RemoteVersion
	var resp FactsResponse
	if err := remoteCall(ctx, r, xxyiMsgPath, SubcmdFacts, req, &resp); err != nil {
		return FactsResponse{}, err
	}
	if resp.Version != RemoteVersion {
		return FactsResponse{}, fmt.Errorf("响应协议版本不符：%d", resp.Version)
	}
	if resp.Error != "" {
		return FactsResponse{}, errors.New("对方采集失败: " + resp.Error)
	}
	if len(resp.Worktrees) == 0 || resp.Branches == nil || resp.Agents == nil {
		return FactsResponse{}, errors.New("远程采集响应缺少必需字段（Worktrees / Branches / Agents）")
	}
	if (resp.ScopeError == "") != (len(resp.ScopeReasons) == 0) {
		return FactsResponse{}, errors.New("远程采集响应的 ScopeError 与 ScopeReasons 不一致")
	}
	return resp, nil
}

// RemoteApply 经 r 在对方机器上执行一步更新。业务失败（OK=false）不是 error，看 ErrKind/Error。
// 返回 error（通信失败、退出码非 0、响应不合法）时远端可能已经写入，结果不确定：
// 调用方必须停下并重新确认现场（重新采集），不得假定未写入，更不得盲目重试。
func RemoteApply(ctx context.Context, r RemoteRunner, xxyiMsgPath string, req ApplyRequest) (ApplyResponse, error) {
	req.Version = RemoteVersion
	var resp ApplyResponse
	if err := remoteCall(ctx, r, xxyiMsgPath, SubcmdApply, req, &resp); err != nil {
		return ApplyResponse{}, err
	}
	if resp.Version != RemoteVersion {
		return ApplyResponse{}, fmt.Errorf("响应协议版本不符：%d", resp.Version)
	}
	if err := validateApplyResponse(req, resp); err != nil {
		return ApplyResponse{}, fmt.Errorf("远程响应不合法（远端结果不确定，需重新确认现场）: %w", err)
	}
	return resp, nil
}
