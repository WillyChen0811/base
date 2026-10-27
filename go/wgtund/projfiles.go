package wgtund

// projfiles.go：项目文件同步（projfiles）接进 wgtund——配置、设置文件、定时、网络状态（T11a）。
//
// 一轮的实际工作全在 projsync.PFSched（它不知道 SSH）；这里只负责：
//   - hosts.json 的 projfiles 段（与 projsync 同级，校验方式照 prepareProjsync）：
//     有这段 → 段里的值优先（remote_root 仍设置文件优先，见 f328455c），enabled=false 整体关掉；
//     没这段 → 服务照样创建、接口照样挂（XXYI 的窗口要能显示「没启用/缺什么」），host/remote_root/remote_meta
//     每轮从设置文件取，总开关是设置文件的 enabled（缺省关：服务空转、不连 Ubuntu）；
//   - 每轮开始读 ~/.xxyi/projfiles-settings.json（Python 服务写），转成 PFSchedSettings 调 SetSettings、
//     限速调 PFTransfer.SetLimit；文件不在、读不了、JSON 或某个键不合法 → 整份回落缺省值并在状态里报告；
//   - 定时：网好每 poll_seconds、热点或未知每 poll_seconds_expensive、断网不跑；启动后 10 秒先跑一轮；
//     每 6 小时一次 RunRound(Full)；touched 去抖到期（OnTouchedDue）立刻跑；同一时间只跑一轮；
//   - 网络状态：对外只给 setNet，网络从差变好时立刻触发一轮（HTTP 接口归 T11b）；
//   - 最近一轮报告、启动恢复结果、设置读取问题原子写 ~/.xxyi/projfiles/status/_round.json。
//
// 并发：SSH 远程执行（sshRemoteRunner）每次调用都在共用连接上另开会话，本身可并发——传输核验与 Apply 同时调
// 不需要额外加锁；SFTP 会话由本服务开一条全程共用（sftp.Client 支持并发请求），断线重连后换新的。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
	"github.com/pkg/sftp"
)

const (
	// projfilesInitialDelay 是 wgtund 启动后第一轮的等待时间。
	projfilesInitialDelay = 10 * time.Second
	// projfilesFullInterval 是定期全量核对的间隔。
	projfilesFullInterval = 6 * time.Hour
	// projfilesRoundTimeout 是单轮的整体上限；到点只放弃等待（RunRound 自己也尊重 ctx）。
	projfilesRoundTimeout = 20 * time.Minute
	// projfilesNetStale 是网络上报的有效期，超过它按「未知」（热点）对待；与 projsync 的缺省一致。
	projfilesNetStale = 3 * time.Minute

	projfilesMinPollSeconds = 5     // poll_seconds 的下限，小于它按它算
	projfilesMaxPollSeconds = 86400 // 秒数上限（一天）
	projfilesMaxRecycleDays = 3650  // 与 Ubuntu 的 recycle_clean 一致

	projfilesMetaDirName    = "projfiles"
	projfilesSettingsName   = "projfiles-settings.json"
	projfilesPathsListName  = "projfiles-paths.json" // XXYI 导出的配置路径清单（§14.1/§14.4，开始同步的闸用它）
	projfilesStatusFileName = "_round.json"
	defaultProjfilesXxyiMsg = "~/.local/bin/xxyi-msg"
)

// 设置文件的缺省值（文件不在或不合法时用；键没写也用它们）。
const (
	projfilesDefaultPollSeconds          = 60
	projfilesDefaultPollExpensiveSeconds = 300
	projfilesDefaultMaxExpensiveSeconds  = 20
	projfilesDefaultMaxUserSeconds       = 60
	projfilesDefaultRecycleDays          = 30
	projfilesDefaultQuietSeconds         = 10
)

// projfilesConf 是 hosts.json 的 projfiles 段（与 projsync 同级）。
type projfilesConf struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host"`        // 必须是 hosts 里的主机，且有 identity_files
	RemoteRoot string `json:"remote_root"` // Ubuntu 的同步根（绝对路径）
	RemoteMeta string `json:"remote_meta"` // Ubuntu 的 meta 目录（绝对路径）
	XxyiMsg    string `json:"xxyi_msg"`    // Ubuntu 上 xxyi-msg 的路径，缺省 ~/.local/bin/xxyi-msg
	CoordID    string `json:"coord_id"`    // 协调者编号，缺省用本机主机名（不合规的字符换成 -）
}

// projfilesPrep 是校验通过后的 projfiles 配置。
type projfilesPrep struct {
	enabled       bool
	host          string
	remoteRoot    string
	remoteMeta    string
	xxyiMsg       string
	coordID       string
	coordHost     string // 给人看的主机名（原样）
	metaDir       string // ~/.xxyi/projfiles：units.json、staging、回收、status 都在里面
	settingsPath  string // ~/.xxyi/projfiles-settings.json
	pathsListPath string // ~/.xxyi/projfiles-paths.json：XXYI 导出的配置路径清单（§14.4 开始同步的闸）
	statusPath    string // ~/.xxyi/projfiles/status/_round.json
	// fromSettings：hosts.json 没写 projfiles 段——host/remote_root/remote_meta 不再固定在 prep 里，
	// 每轮从设置文件取（effRemote），缺项/不合法这一轮不跑并写明原因；总开关是设置文件的 enabled（缺省关）。
	fromSettings bool
	hosts        map[string]hostConf // 没这段时用来每轮校验 host：必须在 hosts 里且有 identity_files
}

var (
	// 与 projsync.remoteCommand 同一字符集；启动时就挡住，不等第一轮才报错。
	projfilesMsgRe = regexp.MustCompile(`^[A-Za-z0-9._/~-]+$`)
	// 与 projsync 的 coord_id 规则一致。
	projfilesCoordRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// prepareProjfiles 校验 projfiles 段并补缺省。没配这段不再返回 nil，而是返回 fromSettings 模式的 prep：
// 服务和 /projfiles/* 照样起（XXYI 的设置窗口要能显示「没启用/缺什么」），host/remote_root/remote_meta
// 每轮从设置文件取，总开关是设置文件里的 enabled（缺省 false）。错误信息以「hosts.json: projfiles」开头。
func prepareProjfiles(c *fileConf) (*projfilesPrep, error) {
	pc := c.Projfiles
	if pc == nil {
		// 没这段：路径与缺省照有这段时算好；host/remote_root/remote_meta 留空，每轮由设置文件补。
		dir, err := xxyiDir()
		if err != nil {
			return nil, fmt.Errorf("hosts.json: projfiles %v", err)
		}
		coordHost, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("hosts.json: projfiles 取不到本机主机名（请补 projfiles 段并写 coord_id）: %v", err)
		}
		coordID := projfilesCoordFromHost(coordHost)
		if coordID == "" {
			return nil, fmt.Errorf("hosts.json: projfiles 本机主机名 %q 整理不成 coord_id（请补 projfiles 段并写 coord_id）", coordHost)
		}
		hosts := make(map[string]hostConf, len(c.Hosts))
		for i := range c.Hosts {
			hosts[c.Hosts[i].Name] = c.Hosts[i]
		}
		return &projfilesPrep{
			fromSettings: true, hosts: hosts,
			xxyiMsg: defaultProjfilesXxyiMsg, coordID: coordID, coordHost: coordHost,
			metaDir:       filepath.Join(dir, projfilesMetaDirName),
			settingsPath:  filepath.Join(dir, projfilesSettingsName),
			pathsListPath: filepath.Join(dir, projfilesPathsListName),
			statusPath:    filepath.Join(dir, projfilesMetaDirName, "status", projfilesStatusFileName),
		}, nil
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("hosts.json: projfiles "+format, a...)
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
		return nil, bad("的主机 %q 没有 identity_files，SSH 无法选私钥", host.Name)
	}
	for _, f := range []struct{ name, v string }{{"remote_root", pc.RemoteRoot}, {"remote_meta", pc.RemoteMeta}} {
		if f.v == "" || !strings.HasPrefix(f.v, "/") || strings.Contains(f.v, "..") || !projRemoteRe.MatchString(f.v) {
			return nil, bad("的 %s %q 不合规：必须以 / 开头、只允许字母数字和 . _ / -、不能含 ..", f.name, f.v)
		}
	}
	xxyiMsg := pc.XxyiMsg
	if xxyiMsg == "" {
		xxyiMsg = defaultProjfilesXxyiMsg
	}
	if !projfilesMsgRe.MatchString(xxyiMsg) || strings.HasPrefix(xxyiMsg, "-") {
		return nil, bad("的 xxyi_msg %q 不合规：只允许 [A-Za-z0-9._/~-]，且不能以 - 开头", xxyiMsg)
	}
	coordHost, err := os.Hostname()
	if err != nil {
		return nil, bad("取不到本机主机名（请显式写 coord_id）: %v", err)
	}
	coordID := pc.CoordID
	if coordID == "" {
		if coordID = projfilesCoordFromHost(coordHost); coordID == "" {
			return nil, bad("本机主机名 %q 整理不成 coord_id，请显式写 coord_id", coordHost)
		}
	} else if !projfilesCoordRe.MatchString(coordID) {
		return nil, bad("的 coord_id %q 不合规：首字符是字母或数字，其余只允许字母数字和 _ -，长度 1..64", coordID)
	}
	dir, err := xxyiDir()
	if err != nil {
		return nil, bad("%v", err)
	}
	return &projfilesPrep{
		enabled: pc.Enabled, host: host.Name,
		remoteRoot: projfilesCleanRemote(pc.RemoteRoot), remoteMeta: projfilesCleanRemote(pc.RemoteMeta),
		xxyiMsg: xxyiMsg, coordID: coordID, coordHost: coordHost,
		metaDir:       filepath.Join(dir, projfilesMetaDirName),
		settingsPath:  filepath.Join(dir, projfilesSettingsName),
		pathsListPath: filepath.Join(dir, projfilesPathsListName),
		statusPath:    filepath.Join(dir, projfilesMetaDirName, "status", projfilesStatusFileName),
	}, nil
}

// projfilesCleanRemote 去掉远端路径结尾多余的 /（Ubuntu 端按 path.Clean 比对，留着的尾巴会让暂存区路径对不上）。
func projfilesCleanRemote(p string) string {
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// projfilesCoordFromHost 把主机名整理成合法的 coord_id：不合规的字符换成 -，去掉开头的 -/_，截到 64。
// 整不成合法的（全空）返回空串，由调用方要求显式配置。
func projfilesCoordFromHost(h string) string {
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.TrimLeft(b.String(), "-_")
	if len(s) > 64 {
		s = s[:64]
	}
	if !projfilesCoordRe.MatchString(s) {
		return ""
	}
	return s
}

// projfilesSettings 是 ~/.xxyi/projfiles-settings.json（Python 服务写）。
type projfilesSettings struct {
	Enabled              bool   `json:"enabled"`
	WorkDir              string `json:"work_dir"`    // 桌面 Projects（单元搬运器的工作目录）
	SyncDir              string `json:"sync_dir"`    // 正位目录
	RemoteRoot           string `json:"remote_root"` // Ubuntu 同步根（绝对路径）；有段时空 = 用 hosts.json 里的（兼容老配置）
	Host                 string `json:"host"`        // hosts.json 里的主机名；只在没这段时生效（每轮校验）
	RemoteMeta           string `json:"remote_meta"` // Ubuntu 同步记录目录（绝对路径）；只在没这段时生效
	PollSeconds          int    `json:"poll_seconds"`
	PollSecondsExpensive int    `json:"poll_seconds_expensive"`
	MaxSecondsExpensive  int    `json:"max_seconds_expensive"`
	MaxSecondsUser       int    `json:"max_seconds_user"`
	LimitKbpsGood        int64  `json:"limit_kbps_good"`
	LimitKbpsExpensive   int64  `json:"limit_kbps_expensive"`
	RecycleDays          int    `json:"recycle_days"`
	QuietSeconds         int    `json:"quiet_seconds"`
}

// projfilesDefaults 是设置文件的缺省值。enabled 缺省开：真正的总开关在 hosts.json 的 projfiles.enabled。
func projfilesDefaults() projfilesSettings {
	return projfilesSettings{
		Enabled:     true,
		PollSeconds: projfilesDefaultPollSeconds, PollSecondsExpensive: projfilesDefaultPollExpensiveSeconds,
		MaxSecondsExpensive: projfilesDefaultMaxExpensiveSeconds, MaxSecondsUser: projfilesDefaultMaxUserSeconds,
		RecycleDays: projfilesDefaultRecycleDays, QuietSeconds: projfilesDefaultQuietSeconds,
	}
}

// loadProjfilesSettings 读设置文件。文件不在、读不了、JSON 不合法 → 整份回落缺省值；
// 单个键不合法 → 只有那个键回落。问题一条条记下来给状态用（绝不让服务起不来）。
func loadProjfilesSettings(path string) (projfilesSettings, []string) {
	def := projfilesDefaults()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return def, []string{fmt.Sprintf("设置文件 %s 不存在，全部按缺省值", path)}
		}
		return def, []string{fmt.Sprintf("读设置文件 %s 失败，全部按缺省值：%v", path, err)}
	}
	var raw struct {
		Enabled              *bool   `json:"enabled"`
		WorkDir              *string `json:"work_dir"`
		SyncDir              *string `json:"sync_dir"`
		RemoteRoot           *string `json:"remote_root"`
		Host                 *string `json:"host"`
		RemoteMeta           *string `json:"remote_meta"`
		PollSeconds          *int    `json:"poll_seconds"`
		PollSecondsExpensive *int    `json:"poll_seconds_expensive"`
		MaxSecondsExpensive  *int    `json:"max_seconds_expensive"`
		MaxSecondsUser       *int    `json:"max_seconds_user"`
		LimitKbpsGood        *int64  `json:"limit_kbps_good"`
		LimitKbpsExpensive   *int64  `json:"limit_kbps_expensive"`
		RecycleDays          *int    `json:"recycle_days"`
		QuietSeconds         *int    `json:"quiet_seconds"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return def, []string{fmt.Sprintf("设置文件 %s 不是合法的 JSON，全部按缺省值：%v", path, err)}
	}
	st, problems := def, []string{}
	if raw.Enabled != nil {
		st.Enabled = *raw.Enabled
	}
	secs := []struct {
		name string
		in   *int
		out  *int
		min  int
	}{
		{"poll_seconds", raw.PollSeconds, &st.PollSeconds, projfilesMinPollSeconds},
		{"poll_seconds_expensive", raw.PollSecondsExpensive, &st.PollSecondsExpensive, projfilesMinPollSeconds},
		{"max_seconds_expensive", raw.MaxSecondsExpensive, &st.MaxSecondsExpensive, 0},
		{"max_seconds_user", raw.MaxSecondsUser, &st.MaxSecondsUser, 0},
		{"quiet_seconds", raw.QuietSeconds, &st.QuietSeconds, 0},
	}
	for _, f := range secs {
		if f.in == nil {
			continue
		}
		if *f.in < f.min || *f.in > projfilesMaxPollSeconds {
			problems = append(problems, fmt.Sprintf("%s=%d 不在 %d..%d 之间，按 %d 算", f.name, *f.in, f.min, projfilesMaxPollSeconds, *f.out))
			continue
		}
		*f.out = *f.in
	}
	for _, f := range []struct {
		name string
		in   *int64
		out  *int64
	}{{"limit_kbps_good", raw.LimitKbpsGood, &st.LimitKbpsGood}, {"limit_kbps_expensive", raw.LimitKbpsExpensive, &st.LimitKbpsExpensive}} {
		if f.in == nil {
			continue
		}
		if *f.in < 0 {
			problems = append(problems, fmt.Sprintf("%s=%d 是负数，按 0（不限速）算", f.name, *f.in))
			continue
		}
		*f.out = *f.in
	}
	if raw.RecycleDays != nil {
		if *raw.RecycleDays < 1 || *raw.RecycleDays > projfilesMaxRecycleDays {
			problems = append(problems, fmt.Sprintf("recycle_days=%d 不在 1..%d 之间，按 %d 算", *raw.RecycleDays, projfilesMaxRecycleDays, def.RecycleDays))
		} else {
			st.RecycleDays = *raw.RecycleDays
		}
	}
	for _, f := range []struct {
		name string
		in   *string
		out  *string
	}{{"work_dir", raw.WorkDir, &st.WorkDir}, {"sync_dir", raw.SyncDir, &st.SyncDir}} {
		if f.in == nil || strings.TrimSpace(*f.in) == "" {
			continue
		}
		p := expand(strings.TrimSpace(*f.in))
		if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
			problems = append(problems, fmt.Sprintf("%s=%q 不是绝对路径，这项留空（不自动建项目、不搬正位）", f.name, *f.in))
			continue
		}
		*f.out = filepath.Clean(p)
	}
	// remote_root 是 Ubuntu 上的绝对路径：校验走远端规则（不是本地路径，不做 ~ 展开）。
	// 合法就用它；不合法或没写就留空——留空表示「用 hosts.json 里的 remote_root」（兼容老配置）。
	if raw.RemoteRoot != nil {
		v := strings.TrimSpace(*raw.RemoteRoot)
		switch {
		case v == "":
			// 空 = 交回 hosts.json，不算问题
		case !strings.HasPrefix(v, "/") || strings.Contains(v, "..") || !projRemoteRe.MatchString(v):
			problems = append(problems, fmt.Sprintf("remote_root=%q 不是合法的远端绝对路径（必须以 / 开头、只允许字母数字和 . _ / -、不能含 ..），这项留空改用 hosts.json 里的 remote_root", *raw.RemoteRoot))
		default:
			st.RemoteRoot = projfilesCleanRemote(v)
		}
	}
	// host 与 remote_meta 只在 hosts.json 没这段时生效（见 settingsModeGap）；host 是名字不是路径，
	// 「在不在 hosts 里、有没有私钥」由每轮的 settingsModeGap 拿 hosts 来判，这里只去空白。
	if raw.Host != nil {
		st.Host = strings.TrimSpace(*raw.Host)
	}
	if raw.RemoteMeta != nil {
		v := strings.TrimSpace(*raw.RemoteMeta)
		switch {
		case v == "":
			// 空 = 交回 hosts.json 的 remote_meta（有这段时本来就用段里的），不算问题
		case !strings.HasPrefix(v, "/") || strings.Contains(v, "..") || !projRemoteRe.MatchString(v):
			problems = append(problems, fmt.Sprintf("remote_meta=%q 不合规：必须以 / 开头、只允许字母数字和 . _ / -、不能含 ..", *raw.RemoteMeta))
		default:
			st.RemoteMeta = projfilesCleanRemote(v)
		}
	}
	return st, problems
}

// settingsHasEnabled 只看设置文件里有没有显式写 enabled 键（文件不在/解不开都算没有）。
// 没这段时总开关以设置文件为准、缺省关，不能用 projfilesDefaults 的 true。
func settingsHasEnabled(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return false
	}
	return raw.Enabled != nil
}

// loadSettings 读设置文件；没这段（fromSettings）时 enabled 以设置文件为准：
// 没写这个键（或文件不在/解不开）按 false——服务空转、不连 Ubuntu。有这段时行为一字不变。
func (s *projfilesSvc) loadSettings() (projfilesSettings, []string) {
	st, problems := loadProjfilesSettings(s.prep.settingsPath)
	if s.prep.fromSettings && !settingsHasEnabled(s.prep.settingsPath) {
		st.Enabled = false
	}
	return st, problems
}

// projfilesSFTPSession 是一条绑定了主机的 SFTP 会话：传输器只认这个面，测试可换成假的（记下开给哪台主机）。
// Host 标明这条会话属于哪台主机——换主机后必须重开（见 attachSFTPReal），不能拿旧主机的会话往新主机写。
type projfilesSFTPSession interface {
	Host() string                        // 这条会话属于哪台主机
	Alive() bool                         // 底层 SSH 连接是否还活着（断了要重开）
	Open(p string) (projsync.PFTransferFile, error)
	OpenFile(p string, flag int) (projsync.PFTransferFile, error)
	Stat(p string) (os.FileInfo, error)
	Remove(p string) error
	MkdirAll(p string) error
	ReadDir(p string) ([]os.FileInfo, error)
	Chtimes(p string, atime, mtime time.Time) error
	Close() error
}

// projfilesHostSFTP 用真实 *sftp.Client 实现 projfilesSFTPSession：记住主机名与它借用的 SSH 连接，
// Alive 看那条连接的 Done 通道（连接断了这条会话就该重开）。
type projfilesHostSFTP struct {
	host string
	c    *sftp.Client
	done <-chan struct{} // 底层 sshterm.Client 的 Done
}

func (h *projfilesHostSFTP) Host() string { return h.host }
func (h *projfilesHostSFTP) Alive() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}
func (h *projfilesHostSFTP) Open(p string) (projsync.PFTransferFile, error) { return h.c.Open(p) }
func (h *projfilesHostSFTP) OpenFile(p string, flag int) (projsync.PFTransferFile, error) { return h.c.OpenFile(p, flag) }
func (h *projfilesHostSFTP) Stat(p string) (os.FileInfo, error)                { return h.c.Stat(p) }
func (h *projfilesHostSFTP) Remove(p string) error                              { return h.c.Remove(p) }
func (h *projfilesHostSFTP) MkdirAll(p string) error                           { return h.c.MkdirAll(p) }
func (h *projfilesHostSFTP) ReadDir(p string) ([]os.FileInfo, error)          { return h.c.ReadDir(p) }
func (h *projfilesHostSFTP) Chtimes(p string, atime, mtime time.Time) error   { return h.c.Chtimes(p, atime, mtime) }
func (h *projfilesHostSFTP) Close() error                                      { return h.c.Close() }

// projfilesRemoteFS 把 PFTransferFS 的调用转发到「当前」这条 SFTP 会话：
// 会话还没开成（开机时 Ubuntu 没连上）时一律报「还没连上」，开成之后自动开始用。
// 会话带主机名：整套重建 / 换主机时先关掉旧的再挂新的，绝不拿旧主机的会话往新主机写（评审 14-1）。
type projfilesRemoteFS struct {
	mu   sync.Mutex
	sess projfilesSFTPSession
}

func (r *projfilesRemoteFS) attach(sess projfilesSFTPSession) {
	r.mu.Lock()
	r.sess = sess
	r.mu.Unlock()
}

// cur 返回当前会话（没挂是 nil）；attachSFTPReal 用它判「同一台且还活着就复用」。
func (r *projfilesRemoteFS) cur() projfilesSFTPSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sess
}

func (r *projfilesRemoteFS) get() (projfilesSFTPSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil {
		return nil, errors.New("还没有连上 Ubuntu 的 SFTP")
	}
	return r.sess, nil
}

func (r *projfilesRemoteFS) Open(p string) (projsync.PFTransferFile, error) {
	c, err := r.get()
	if err != nil {
		return nil, err
	}
	return c.Open(p)
}

func (r *projfilesRemoteFS) OpenFile(p string, flag int) (projsync.PFTransferFile, error) {
	c, err := r.get()
	if err != nil {
		return nil, err
	}
	return c.OpenFile(p, flag)
}

func (r *projfilesRemoteFS) Stat(p string) (os.FileInfo, error) {
	c, err := r.get()
	if err != nil {
		return nil, err
	}
	return c.Stat(p)
}

func (r *projfilesRemoteFS) Remove(p string) error {
	c, err := r.get()
	if err != nil {
		return err
	}
	return c.Remove(p)
}

func (r *projfilesRemoteFS) MkdirAll(p string) error {
	c, err := r.get()
	if err != nil {
		return err
	}
	return c.MkdirAll(p)
}

func (r *projfilesRemoteFS) ReadDir(p string) ([]os.FileInfo, error) {
	c, err := r.get()
	if err != nil {
		return nil, err
	}
	return c.ReadDir(p)
}

func (r *projfilesRemoteFS) Chtimes(p string, atime, mtime time.Time) error {
	c, err := r.get()
	if err != nil {
		return err
	}
	return c.Chtimes(p, atime, mtime)
}

func (r *projfilesRemoteFS) Join(elem ...string) string {
	return filepath.ToSlash(filepath.Join(elem...))
}

// projfilesStatus 是 ~/.xxyi/projfiles/status/_round.json 的内容（T11b 的 HTTP 接口直接用同一份）。
type projfilesStatus struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	// HostFrom 是 Host 的来源：hosts（hosts.json 的 projfiles 段）/ settings（设置文件；没这段时每轮取）。
	HostFrom string `json:"host_from"`
	// RemoteRoot 是当前生效的 Ubuntu 同步根（设置里有合法的就报它，否则报 hosts 里的）。
	RemoteRoot     string `json:"remote_root"`
	RemoteRootFrom string `json:"remote_root_from"`
	RemoteMeta     string `json:"remote_meta"`
	// RemoteMetaFrom 是 RemoteMeta 的来源，规则同 HostFrom。
	RemoteMetaFrom string `json:"remote_meta_from"`
	// ConfigGap 是没这段时设置里缺项/不合法的一句话（原文同 last_action 里的原因）；齐全或有这段时空串。
	ConfigGap      string `json:"config_gap,omitempty"`
	CoordID        string `json:"coord_id"`
	CoordHost      string `json:"coord_host"`
	MetaDir        string `json:"meta_dir"`
	SettingsPath   string `json:"settings_path"`

	Net        string     `json:"net"`
	Running    bool       `json:"running"`
	NextRun    *time.Time `json:"next_run"`
	Updated    time.Time  `json:"updated"`
	IntervalS  int        `json:"interval_seconds"`      // 当前网络下用的轮询间隔
	LastAction string     `json:"last_action,omitempty"` // 最近一次到点/唤醒时做了什么或为什么没跑

	Settings         projfilesSettings `json:"settings"`
	LimitBps         int64             `json:"limit_bps"` // 当前生效的限速（字节/秒），两方向同值，0 = 不限
	SettingsProblems []string          `json:"settings_problems,omitempty"`
	Notices          []string          `json:"notices,omitempty"`

	// ActiveWorkDir / ActiveSyncDir：当前实际在用（build 已经用上）的两个目录——
	// 设置可能已经改了但整套重建还没跑完，报这两个才是真的。
	ActiveWorkDir string `json:"active_work_dir,omitempty"`
	ActiveSyncDir string `json:"active_sync_dir,omitempty"`
	// ActiveRemoteRoot / ActiveRemoteRootFrom：当前实际在用（PFRemoteClient 已绑上）的 Ubuntu 同步根与它的来源。
	ActiveRemoteRoot     string `json:"active_remote_root,omitempty"`
	ActiveRemoteRootFrom string `json:"active_remote_root_from,omitempty"`
	// ActiveHost：当前实际在用（runner 已绑上）的主机；设置改了但还没重建完时报的还是旧主机。
	ActiveHost string `json:"active_host,omitempty"`
	// SwitchBusy 非空表示「目录刚改过、整套重建还没完」；这期间 unit/start 等一律拒绝。
	SwitchBusy   bool   `json:"switch_busy,omitempty"`
	SwitchReason string `json:"switch_reason,omitempty"`

	StartedUp    bool                        `json:"started_up"`
	Startup      []projsync.PFUnitMoveResult `json:"startup,omitempty"`
	StartupError string                      `json:"startup_error,omitempty"`

	LastRound      *projsync.PFSchedReport `json:"last_round,omitempty"`
	PendingTouched []string                `json:"pending_touched,omitempty"`
	BuildError     string                  `json:"build_error,omitempty"`
}

// projfilesSvc 是项目文件同步服务：配置 + 零件 + 定时 + 状态。创建后可并发使用（setNet 与 loop 并发）。
type projfilesSvc struct {
	prep *projfilesPrep
	pool *sshPool

	// 注入点（测试换掉）；生产用真实实现。
	now        func() time.Time
	wait       func(ctx context.Context, d time.Duration) (reason string, err error)
	runRound   func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport
	startUp    func(ctx context.Context) ([]projsync.PFUnitMoveResult, error)
	buildMover func(cfg projsync.PFUnitMoveConfig) (*projsync.PFUnitMover, error)
	attachSFTP func() error
	// openSFTP 在指定主机上开一条会话（评审 14-1：会话必须绑主机）；测试换成假的记下开给哪台。
	openSFTP func(host string) (projfilesSFTPSession, error)
	// runner 非 nil 时代替连接池的 SSH 远程执行（测试用假 Runner，不连真机器）。
	runner projsync.RemoteRunner

	wake chan struct{}

	// buildMu 串起「检查 built → 构建 → 发布」，保证第一次 build 并发进来只建出一套；
	// rebuildLocked 也走它，这样并发的 getFresh / tick 不会看到半成品。
	buildMu sync.Mutex

	mu       sync.Mutex
	remote   *projsync.PFRemoteClient
	sftpFS   *projfilesRemoteFS
	transfer *projsync.PFTransfer
	mover    *projsync.PFUnitMover
	sched    *projsync.PFSched
	built    bool
	// builtWork / builtSync：build 发布时实际用的 work_dir/sync_dir，用来对比新一份设置有没有改过目录；
	// 改了就要整套重建（见 tick 的 rebuildLocked 分支）。
	builtWork string
	builtSync string
	// builtRemote / builtRemoteFrom：build 发布时实际用的 Ubuntu 同步根（remote_root）与它的来源
	// （settings = 设置文件里有合法的，hosts = 回落到 hosts.json）。改了同样要整套重建（PFRemoteClient.Root 绑死了它）。
	builtRemote     string
	builtRemoteFrom string
	// builtHost / builtMeta：build 发布时实际用的主机与 Ubuntu 同步记录目录。
	// 有 projfiles 段时恒等于 prep 里的值；没这段时每轮来自设置，改了走同一套切换重建。
	builtHost  string
	builtMeta  string
	// builtGen 每次 build/rebuild 发布成功后 +1；缓存的 PFFresh 靠它认出自己绑的是不是旧一套
	// （旧 sched 会被关掉，缓存不失效就会拿它继续取最新）。
	builtGen atomic.Uint64
	buildErr string
	// switchBusy 非空表示「work_dir 或 sync_dir 刚改过、整套重建还没跑完」：
	// 这时 unit/start / unit/stop / unit/create / fresh 一律拒绝，避免对旧目录继续操作。
	switchBusy string
	startedUp  bool
	startup    []projsync.PFUnitMoveResult
	startupEr  string
	running    bool
	net        string
	netAt      time.Time
	settings   projfilesSettings
	problems   []string
	notices    []string
	lastRound  *projsync.PFSchedReport
	lastFull   time.Time
	nextAt     time.Time
	lastAct    string
}

func newProjfilesSvc(prep *projfilesPrep, pool *sshPool) *projfilesSvc {
	s := &projfilesSvc{prep: prep, pool: pool, now: time.Now, sftpFS: &projfilesRemoteFS{},
		wake: make(chan struct{}, 1), settings: projfilesDefaults()}
	s.wait = s.waitReal
	s.runRound = func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		return s.getSched().RunRound(ctx, opt)
	}
	s.startUp = func(ctx context.Context) ([]projsync.PFUnitMoveResult, error) {
		return s.getSched().PFSchedStartup(ctx)
	}
	s.buildMover = func(cfg projsync.PFUnitMoveConfig) (*projsync.PFUnitMover, error) {
		return projsync.NewPFUnitMover(cfg)
	}
	s.attachSFTP = s.attachSFTPReal
	s.openSFTP = s.openSFTPReal
	return s
}

func (s *projfilesSvc) getSched() *projsync.PFSched {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sched
}

// schedGen 同一把锁里取当前调度器与它的代次：调用方缓存了 sched 之后，
// 靠代次变化发现自己缓存的是已被关掉的旧一套（见 projfilesAPI.getFresh）。
func (s *projfilesSvc) schedGen() (*projsync.PFSched, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sched, s.builtGen.Load()
}

// errText 把 error 变成状态里的文本（nil 是空串）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ── 零件 ────────────────────────────────────────────────

// errProjfilesDirSwitch 是设置里的主机/同步根/同步目录/工作目录跟已经建成的那一套不一致、
// 又不能在调用方那里整套重建时返回的错误：调用方（HTTP）应当回 busy/503，让人稍后再试。
var errProjfilesDirSwitch = errors.New("同步主机/同步根/同步目录/工作目录刚改过，正在切换，稍后再试")

// build 建 Ubuntu 客户端、传输器、调度器。work_dir/sync_dir/remote_root 来自设置文件（remote_root 空时回落 hosts），
// 所以必须在读到设置之后调。全程走 buildMu（fix 2：并发只建一套）：
//   - 已建成且目录（含 remote_root）没改 → 直接复用；
//   - 已建成但目录改了 → 返回 errProjfilesDirSwitch（整套重建是 tick 在 acquire 分支里做的事）；
//   - 还没建成 → 建一套发布出去，并把用过的目录与同步根记进 builtWork/builtSync/builtRemote。
func (s *projfilesSvc) build(st projfilesSettings) error {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	cfg := s.effRemote(st)
	if why := s.settingsModeGap(cfg); why != "" {
		return errors.New(why) // 没这段且设置不齐：不建、也不报错退出（调用方记进状态）
	}
	s.mu.Lock()
	if s.built && s.builtWork == st.WorkDir && s.builtSync == st.SyncDir &&
		s.builtRemote == cfg.remoteRoot && s.builtHost == cfg.host && s.builtMeta == cfg.remoteMeta {
		s.mu.Unlock()
		return nil
	}
	if s.built {
		s.mu.Unlock()
		return errProjfilesDirSwitch
	}
	s.mu.Unlock()
	return s.buildLocked(st)
}

// rebuildLocked 强制按新目录整套重建：调用方必须先 acquire() 占住轮次（tick 已经这样做）。
// 走 buildMu 保证跟并发的 build / getFresh 互不看到半成品；旧的 sched 与 transfer 先关再换。
func (s *projfilesSvc) rebuildLocked(st projfilesSettings) error {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	cfg := s.effRemote(st)
	if why := s.settingsModeGap(cfg); why != "" {
		return errors.New(why)
	}
	s.mu.Lock()
	if !s.built || (s.builtWork == st.WorkDir && s.builtSync == st.SyncDir &&
		s.builtRemote == cfg.remoteRoot && s.builtHost == cfg.host && s.builtMeta == cfg.remoteMeta) {
		s.mu.Unlock()
		return nil // 要么还没建成（build() 处理），要么目录/同步根/主机没变（不必重建）
	}
	s.mu.Unlock()
	return s.buildLocked(st)
}

// buildLocked 实际造零件并发布；调用方持有 buildMu。
func (s *projfilesSvc) buildLocked(st projfilesSettings) error {
	runner := s.runner // 测试注入；生产为 nil，用共用连接池的 SSH 远程执行
	if runner == nil {
		if s.pool == nil {
			return errors.New("没有 SSH 连接池")
		}
		runner = newSSHRemoteRunner(s.pool, s.effRemote(st).host)
	}
	// 本轮生效的一套远端配置：有 projfiles 段时段里优先（remote_root 设置文件优先）；
	// 没这段时 host/remote_root/remote_meta 全来自设置文件（build 已挡过缺项，这里一定齐）。
	cfg := s.effRemote(st)
	remote := &projsync.PFRemoteClient{
		Runner: runner, XxyiMsg: s.prep.xxyiMsg,
		Meta: cfg.remoteMeta, Root: cfg.remoteRoot, CoordID: s.prep.coordID, CoordHost: s.prep.coordHost,
	}
	transfer, err := projsync.NewPFTransfer(projsync.PFTransferConfig{
		LocalStaging:  filepath.Join(s.prep.metaDir, "staging"),
		RemoteStaging: projsync.PFRemoteStagingDir(cfg.remoteMeta),
		Remote:        s.sftpFS,
		VerifyUp:      remote.TargetVerify(),
	})
	if err != nil {
		return err
	}
	var mover *projsync.PFUnitMover
	switch {
	case st.WorkDir == "" || st.SyncDir == "":
		s.noteOnce("设置里没有 work_dir/sync_dir：不自动建项目、不搬正位")
	default:
		m, err := s.buildMover(projsync.PFUnitMoveConfig{
			WorkDir: st.WorkDir, SyncDir: st.SyncDir, Meta: s.prep.metaDir,
			QuietSeconds: st.QuietSeconds, Now: s.now,
			PathsList: s.prep.pathsListPath, // §14.4 开始同步的闸：清单读不到时 StartSync 会拒绝并写原因
		})
		if err != nil {
			s.noteOnce(fmt.Sprintf("建单元搬运器失败（不自动建项目、不搬正位；设置改好后整套重建再生效）：%v", err))
		} else {
			mover = m
		}
	}
	sched, err := projsync.NewPFSched(projsync.PFSchedConfig{
		MetaDir: s.prep.metaDir, WorkDir: st.WorkDir, Remote: remote, Transfer: transfer, Mover: mover,
		Settings: s.schedSettings(st), Now: s.now,
		OnTouchedDue: func([]string) { s.kick() },
	})
	if err != nil {
		transfer.Close()
		return err
	}
	s.mu.Lock()
	oldSched, oldTransfer := s.sched, s.transfer
	s.remote, s.transfer, s.mover, s.sched = remote, transfer, mover, sched
	s.built, s.buildErr = true, ""
	s.builtWork, s.builtSync = st.WorkDir, st.SyncDir
	s.builtRemote, s.builtRemoteFrom = cfg.remoteRoot, cfg.rootFrom
	s.builtHost, s.builtMeta = cfg.host, cfg.remoteMeta
	// 每发布一套新零件 +1：缓存的 PFFresh（绑旧 sched）据此作废重建。跟发布同一把锁里加，
	// 保证「拿到 sched」和「拿到代次」是同一时刻的快照。
	s.builtGen.Add(1)
	s.mu.Unlock()
	// 旧的调度器与传输器整套关掉（rebuild 时 baseCtx 取消会让 in-flight 那一轮和 touched 去抖都停下来）。
	if oldSched != nil {
		oldSched.Close()
	}
	if oldTransfer != nil {
		oldTransfer.Close()
	}
	// 评审 14-1：整套重建（多为换了主机）时把旧的 SFTP 会话也关掉清空——传输器绑的还是同一个 sftpFS，
	// 不关掉就会拿旧主机的会话往新主机写。关掉后由本轮 acquire 之后的 attachSFTP 在新生效主机上重开。
	s.detachSFTP()
	// 新建的传输器立刻按当前网络挂上限：启动恢复那一轮在第一次 tick 之前就会传文件。
	s.applyLimits(st, s.effNet())
	return nil
}

// needRebuild 看当前设置里的 work_dir/sync_dir/生效的 remote_root/host/remote_meta
// 是不是跟 build 用的那一份不同（没这段时后三者来自设置文件，改了同样要整套重建）。
func (s *projfilesSvc) needRebuild(st projfilesSettings) bool {
	cfg := s.effRemote(st)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.built && (s.builtWork != st.WorkDir || s.builtSync != st.SyncDir ||
		s.builtRemote != cfg.remoteRoot || s.builtHost != cfg.host || s.builtMeta != cfg.remoteMeta)
}

// switchPending 看服务是不是正在「目录改了但还没整套重建完」的切换期；HTTP 接口用它决定要不要拒绝写请求。
func (s *projfilesSvc) switchPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchBusy != ""
}

// switchReason 是切换期给人看的一句话。
func (s *projfilesSvc) switchReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchBusy
}

// activeDirs 报当前实际在用的 work_dir/sync_dir（状态接口用）。
func (s *projfilesSvc) activeDirs() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builtWork, s.builtSync
}

// activeRemote 报当前实际在用（build 已经绑上 PFRemoteClient）的 Ubuntu 同步根与它的来源（状态接口用）。
// 没建成时是空串/空来源。
func (s *projfilesSvc) activeRemote() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builtRemote, s.builtRemoteFrom
}

// effRemoteRoot 算一份设置下「该用哪个」Ubuntu 同步根：设置文件里有合法的就用它（来源 settings），
// 空则回落到 hosts.json 里的（来源 hosts）。prep 在 prepare 之后不再变，无需加锁。
// loadProjfilesSettings 已把不合法的 remote_root 清成空串，所以这里的非空值一定合法。
func (s *projfilesSvc) effRemoteRoot(st projfilesSettings) (string, string) {
	if st.RemoteRoot != "" {
		return st.RemoteRoot, "settings"
	}
	return s.prep.remoteRoot, "hosts"
}

// projfilesRemoteCfg 是一份设置下「该用哪套」远端配置（主机、同步根、同步记录目录）与各自来源。
type projfilesRemoteCfg struct {
	host       string
	hostFrom   string // hosts（hosts.json 的 projfiles 段）/ settings（设置文件；没这段时）
	remoteRoot string
	rootFrom   string // settings（设置文件里有合法的）/ hosts（回落 hosts.json；没这段时恒 settings）
	remoteMeta string
	metaFrom   string // 同 hostFrom
}

// effRemote 汇总一份设置下生效的远端三件套：有 projfiles 段时 host/remote_meta 用段里的
// （remote_root 仍设置文件优先，照现有规则）；没这段时三项都取设置文件（不合法的值 load 已清成空）。
func (s *projfilesSvc) effRemote(st projfilesSettings) projfilesRemoteCfg {
	if s.prep.fromSettings {
		return projfilesRemoteCfg{host: st.Host, hostFrom: "settings",
			remoteRoot: st.RemoteRoot, rootFrom: "settings",
			remoteMeta: st.RemoteMeta, metaFrom: "settings"}
	}
	root, rootFrom := s.effRemoteRoot(st)
	return projfilesRemoteCfg{host: s.prep.host, hostFrom: "hosts",
		remoteRoot: root, rootFrom: rootFrom,
		remoteMeta: s.prep.remoteMeta, metaFrom: "hosts"}
}

// settingsModeGap 是没这段（fromSettings）时设置里缺项/不合法的第一句原因；齐全（或有这段）返回空串。
// 缺项/不合法 → 这一轮不跑，原因原文写进状态（LastAction / ConfigGap），不报错退出。
func (s *projfilesSvc) settingsModeGap(cfg projfilesRemoteCfg) string {
	if !s.prep.fromSettings {
		return ""
	}
	if cfg.host == "" {
		return "设置里没有 host"
	}
	h, ok := s.prep.hosts[cfg.host]
	if !ok {
		return fmt.Sprintf("设置里的 host %q 不在 hosts.json 里", cfg.host)
	}
	if len(h.IdentityFiles) == 0 {
		return fmt.Sprintf("设置里的 host %q 没有 identity_files，SSH 无法选私钥", cfg.host)
	}
	if cfg.remoteRoot == "" {
		return "设置里没有 remote_root"
	}
	if cfg.remoteMeta == "" {
		return "设置里没有 remote_meta"
	}
	return ""
}

// activeHost 报当前实际在用（build 已经绑上 runner）的主机；还没建成时回落到 prep 里的（没这段时为空）。
func (s *projfilesSvc) activeHost() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.builtHost != "" {
		return s.builtHost
	}
	return s.prep.host
}

// markSwitchBusy 把服务标成切换期，reason 给人看。
func (s *projfilesSvc) markSwitchBusy(reason string) {
	s.mu.Lock()
	s.switchBusy = reason
	s.mu.Unlock()
}

// clearSwitchBusy 解除切换期。
func (s *projfilesSvc) clearSwitchBusy() {
	s.mu.Lock()
	s.switchBusy = ""
	s.mu.Unlock()
}

// attachSFTPReal 把「当前生效主机」的 SFTP 会话挂到传输器上，全程共用：
//   - 已经挂着同一台、底层连接还活着 → 复用，不重开；
//   - 主机换了、或还没开会话、或底层连接断了 → 先关掉旧的，再在新生效的主机上开一条挂上。
//
// 关键（评审 14-1）：换主机后不能让新一轮继续用旧主机的会话往旧机器写——会话带主机名，这里逐轮核对。
// 主机取当前实际生效的那套（没 projfiles 段时 host 来自设置文件，切换后跟着换）。
func (s *projfilesSvc) attachSFTPReal() error {
	host := s.activeHost()
	if host == "" {
		return errors.New("还不知道要连哪台 Ubuntu（设置里没有 host，也没有 hosts.json）")
	}
	if cur := s.sftpFS.cur(); cur != nil && cur.Host() == host && cur.Alive() {
		return nil // 同一台、连接还活着 → 复用这条会话
	}
	s.detachSFTP() // 换主机 / 重连：关掉旧会话再开新的
	sess, err := s.openSFTP(host)
	if err != nil {
		return err
	}
	s.sftpFS.attach(sess)
	return nil
}

// openSFTPReal 在共用连接池的指定主机上开一条 SFTP 会话，包成带主机名的会话。连接已断则丢弃重取。
func (s *projfilesSvc) openSFTPReal(host string) (projfilesSFTPSession, error) {
	if s.pool == nil {
		return nil, errors.New("没有 SSH 连接池")
	}
	c, err := s.pool.conn(host)
	if err != nil {
		return nil, err
	}
	select {
	case <-c.Done():
		s.pool.drop(host, c) // 连接已断：丢掉重取一条
		if c, err = s.pool.conn(host); err != nil {
			return nil, err
		}
	default:
	}
	cl, err := c.SFTP()
	if err != nil {
		return nil, err
	}
	return &projfilesHostSFTP{host: host, c: cl, done: c.Done()}, nil
}

// ── 网络 ────────────────────────────────────────────────

// setNet 是唯一对外的网络入口（HTTP 接口归 T11b）：转给 PFSched.SetNet，网络从差变好时立刻触发一轮；
// 网络一变就把对应的限速刷到传输器上（断网不动限速，跟 tick 里的门槛一致）。
func (s *projfilesSvc) setNet(net string, at time.Time) {
	if !projfilesNetValid(net) {
		net = projsync.PFRemoteNetUnknown
	}
	s.mu.Lock()
	wasGood := s.effNetLocked() == projsync.PFRemoteNetGood
	s.net, s.netAt = net, at
	becameGood := s.effNetLocked() == projsync.PFRemoteNetGood
	eff := s.effNetLocked()
	sched, st := s.sched, s.settings
	s.mu.Unlock()
	if sched != nil {
		sched.SetNet(net, at)
	}
	if eff != projsync.PFRemoteNetOffline {
		s.applyLimits(st, eff)
	}
	if becameGood && !wasGood {
		s.kick()
	}
}

func projfilesNetValid(net string) bool {
	switch net {
	case projsync.PFRemoteNetGood, projsync.PFRemoteNetExpensive, projsync.PFRemoteNetUnknown, projsync.PFRemoteNetOffline:
		return true
	}
	return false
}

// effNetLocked 按上报时间算现在该用的网络（过期按未知）。调用方持锁。
func (s *projfilesSvc) effNetLocked() string {
	if s.net == "" || (!s.netAt.IsZero() && s.now().Sub(s.netAt) > projfilesNetStale) {
		return projsync.PFRemoteNetUnknown
	}
	return s.net
}

func (s *projfilesSvc) effNet() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effNetLocked()
}

// kick 投一次唤醒（不阻塞；已经在等就不重复）。
func (s *projfilesSvc) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ── 设置与参数 ──────────────────────────────────────────

// schedSettings 把设置转成调度器参数；限速不在这里（走传输器的 SetLimit）。
func (s *projfilesSvc) schedSettings(st projfilesSettings) projsync.PFSchedSettings {
	return projsync.PFSchedSettings{
		Limits: projsync.PFRoundLimits{
			MaxExpensive: time.Duration(st.MaxSecondsExpensive) * time.Second,
			MaxUser:      time.Duration(st.MaxSecondsUser) * time.Second,
		},
		Host:         s.prep.coordHost,
		QuietSeconds: st.QuietSeconds,
		FullInterval: projfilesFullInterval,
	}
}

// applyLimits 按当前网络把限速落到传输器上（两个方向同一个值）；kbps→字节/秒，0 = 不限。
func (s *projfilesSvc) applyLimits(st projfilesSettings, net string) int64 {
	kbps := st.LimitKbpsGood
	if net != projsync.PFRemoteNetGood {
		kbps = st.LimitKbpsExpensive
	}
	bps := kbps * 1024
	s.mu.Lock()
	tr := s.transfer
	s.mu.Unlock()
	if tr != nil {
		for _, d := range []projsync.PFDirection{projsync.PFUpload, projsync.PFDownload} {
			tr.SetLimit(d, bps)
		}
	}
	return bps
}

// ── 定时 ────────────────────────────────────────────────

// waitReal 等 d：ctx 取消返回 ctx.Err()，被唤醒 reason="wake"，到点 reason="timer"。
func (s *projfilesSvc) waitReal(ctx context.Context, d time.Duration) (string, error) {
	select {
	case <-s.wake:
		return "wake", nil
	default:
	}
	if d < 0 {
		d = 0
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.wake:
		return "wake", nil
	case <-t.C:
		return "timer", nil
	}
}

// planWait 算还要等多久，并刷新 nextAt；断网也按贵的间隔继续探测设置与网络变化。
func (s *projfilesSvc) planWait(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextAt.IsZero() {
		s.nextAt = now.Add(projfilesInitialDelay)
	}
	if d := s.nextAt.Sub(now); d > 0 {
		return d
	}
	return 0
}

// intervalLocked 是当前网络该用的轮询间隔。调用方持锁。
func (s *projfilesSvc) intervalLocked(st projfilesSettings) time.Duration {
	sec := st.PollSeconds
	if s.effNetLocked() != projsync.PFRemoteNetGood {
		sec = st.PollSecondsExpensive
	}
	if sec < projfilesMinPollSeconds {
		sec = projfilesMinPollSeconds
	}
	return time.Duration(sec) * time.Second
}

// loop 是定时调度：有 projfiles 段且 enabled=false 时立即返回（照旧）；没这段时照样跑，
// 每轮由设置决定跑不跑（enabled 缺省关、缺项不跑，见 tick），这样状态一直是活的。
func (s *projfilesSvc) loop(ctx context.Context) {
	if s.prep == nil || (!s.prep.fromSettings && !s.prep.enabled) {
		return
	}
	s.startupOnce(ctx)
	for {
		reason, err := s.wait(ctx, s.planWait(s.now()))
		if err != nil {
			return
		}
		s.tick(ctx, reason)
		s.mu.Lock()
		s.nextAt = s.now().Add(s.intervalLocked(s.settings))
		s.mu.Unlock()
		s.persist()
	}
}

// startupOnce 启动时把没挪完的项目恢复掉（只一次），结果进状态。
func (s *projfilesSvc) startupOnce(ctx context.Context) {
	st, problems := s.loadSettings()
	s.applySettings(st, problems)
	if why := s.settingsModeGap(s.effRemote(st)); why != "" {
		s.setAction("启动：没跑（" + why + "）")
		s.persist()
		return
	}
	if err := s.build(st); err != nil {
		s.setBuildError(err)
		s.setAction("启动：零件没建成，" + err.Error())
		return
	}
	s.mu.Lock()
	fn, done := s.startUp, s.startedUp
	s.mu.Unlock()
	if done || fn == nil {
		return
	}
	res, err := fn(ctx)
	act := "启动恢复：没有没挪完的项目"
	switch {
	case err != nil:
		act = "启动恢复失败：" + err.Error()
	case len(res) > 0:
		act = fmt.Sprintf("启动恢复：%d 个没挪完的项目已处理", len(res))
	}
	s.mu.Lock()
	s.startedUp, s.startup, s.startupEr, s.lastAct = true, res, errText(err), act
	s.mu.Unlock()
	s.persist()
}

// tick 是到点或被唤醒之后的动作：读设置 → 若目录改了进入切换期 → 该跑就跑一轮（必要时先整套重建）→ 写状态。
func (s *projfilesSvc) tick(ctx context.Context, reason string) {
	now := s.now()
	st, problems := s.loadSettings()
	s.applySettings(st, problems)
	// 没这段时设置不齐（缺 host / host 不在 hosts / 没私钥 / 远端路径缺项）：这一轮不跑，
	// 原因原文写进状态（ConfigGap + last_action），不进切换期、不报错退出。
	if why := s.settingsModeGap(s.effRemote(st)); why != "" {
		s.setAction("没跑：" + why)
		s.persist()
		return
	}
	// 目录、同步根或主机改了：先进切换期，unit/start / unit/stop / unit/create / fresh 都拒绝，
	// 直到本轮 acquire 之后 rebuildLocked 完成。
	if s.needRebuild(st) {
		s.markSwitchBusy("同步主机/同步根/同步目录/工作目录刚改过，正在切换，稍后再试")
	}
	if err := s.build(st); err != nil && err != errProjfilesDirSwitch {
		s.setBuildError(err)
		s.setAction("没跑：零件还没建成（" + err.Error() + "）")
		s.persist()
		return
	}
	if !st.Enabled {
		s.setAction("没跑：设置里 enabled=false")
		s.persist()
		return
	}
	if net := s.effNet(); net == projsync.PFRemoteNetOffline {
		s.setAction("没跑：断网")
		s.persist()
		return
	}
	net := s.effNet()
	opt, run := s.roundOpt(now)
	switch {
	case !run:
		s.setAction("没跑：没有要处理的")
	case !s.acquire():
		s.setAction("没跑：上一轮还在进行")
	default:
		// 已经占住轮次：这时没有任何轮次和搬运在跑，可以整套重建。
		if s.switchPending() {
			if err := s.rebuildLocked(st); err != nil {
				s.release()
				s.setBuildError(err)
				s.setAction("没跑：整套重建没成功（" + err.Error() + "）")
				s.persist()
				return
			}
			s.clearSwitchBusy()
			s.noteOnce("同步主机/同步根/同步目录/工作目录改了，已整套重建")
		}
		// 会话在重建之后才开（评审 14-1）：换主机时 rebuild 已关掉旧会话，这里在新生效的主机上开一条，
		// 保证这一轮往正确的机器传；同一台且还活着则复用，不重开。
		if err := s.attachSFTP(); err != nil {
			s.noteOnce("开 SFTP 会话失败（要传文件时会失败）：" + err.Error())
		}
		sched := s.getSched()
		// SetSettings 是整体替换，会把网络状态一起洗掉，所以紧跟着把已上报的网络补回去。
		sched.SetSettings(s.schedSettings(st))
		s.mu.Lock()
		netAt := s.netAt
		s.mu.Unlock()
		sched.SetNet(net, netAt)
		s.applyLimits(st, net)
		kind := "常规"
		if opt.Full {
			kind = "全量"
		}
		rep := s.execRound(ctx, opt, now)
		s.mu.Lock()
		s.lastRound, s.running = rep, false
		if opt.Full {
			s.lastFull = now
		}
		s.mu.Unlock()
		// 跑完之后才记这一句：中途被别人插进来的「跳过」不该盖在最后结果上
		s.setAction(fmt.Sprintf("跑了：%s一轮（%s）：网络 %s，结果 %s", kind, reason, net, rep.Outcome))
	}
	s.persist()
}

// roundOpt 决定这一轮跑什么：全量到期跑全量；否则有到期的 touched 就只跑那些；再否则常规一轮。
func (s *projfilesSvc) roundOpt(now time.Time) (projsync.PFSchedRoundOptions, bool) {
	s.mu.Lock()
	lastFull, sched := s.lastFull, s.sched
	s.mu.Unlock()
	if sched == nil {
		return projsync.PFSchedRoundOptions{}, false
	}
	if lastFull.IsZero() || now.Sub(lastFull) >= projfilesFullInterval {
		return projsync.PFSchedRoundOptions{Full: true}, true
	}
	if codes := sched.PendingTouched(); len(codes) > 0 {
		return projsync.PFSchedRoundOptions{Only: codes}, true
	}
	return projsync.PFSchedRoundOptions{}, true
}

// execRound 在 goroutine 里跑一轮：超时或退出就放弃等待（只在正常返回后读结果，照 projsync_v2 的写法）。
func (s *projfilesSvc) execRound(ctx context.Context, opt projsync.PFSchedRoundOptions, now time.Time) *projsync.PFSchedReport {
	var rep *projsync.PFSchedReport
	rctx, cancel := context.WithTimeout(ctx, projfilesRoundTimeout)
	defer cancel()
	err := runInterruptible(rctx, interruptGrace, func() {}, func() error {
		rep = s.runRound(rctx, opt)
		return nil
	})
	if err == nil && rep != nil {
		return rep
	}
	out := &projsync.PFSchedReport{StartedAt: now, EndedAt: s.now(), Net: s.effNet()}
	if ctx.Err() != nil {
		out.Outcome, out.Reason = projsync.PFSchedCanceled, "助手正在退出，本轮已取消"
	} else {
		out.Outcome, out.Reason = projsync.PFSchedFailed, fmt.Sprintf("本轮超过 %v 还没结束，已放弃等待：%v", projfilesRoundTimeout, err)
	}
	return out
}

// acquire 占位：同一时间只跑一轮。
func (s *projfilesSvc) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

// ── 状态 ────────────────────────────────────────────────

func (s *projfilesSvc) applySettings(st projfilesSettings, problems []string) {
	s.mu.Lock()
	s.settings, s.problems = st, problems
	s.mu.Unlock()
}

func (s *projfilesSvc) setBuildError(err error) {
	s.mu.Lock()
	if err != nil {
		s.buildErr = err.Error()
	} else {
		s.buildErr = ""
	}
	s.mu.Unlock()
}

// setAction 记下最近一次到点/唤醒时的处置（给人看的一句话）。调用方可以持锁。
func (s *projfilesSvc) setAction(msg string) {
	s.mu.Lock()
	s.lastAct = msg
	s.mu.Unlock()
}

// noteOnce 追加一条给人看的说明（同一句只记一次）。
func (s *projfilesSvc) noteOnce(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.notices {
		if m == msg {
			return
		}
	}
	s.notices = append(s.notices, msg)
}

func (s *projfilesSvc) status() projfilesStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 当前生效的远端三件套（按已读进服务的设置算）与各自来源；没这段时全部来自设置文件，
	// 缺项那句原文进 ConfigGap，窗口直接显示「缺什么」。
	cfg := s.effRemote(s.settings)
	enabled := s.prep.enabled
	if s.prep.fromSettings {
		enabled = s.settings.Enabled
	}
	out := projfilesStatus{
		Enabled: enabled, Host: cfg.host, HostFrom: cfg.hostFrom,
		RemoteRoot: cfg.remoteRoot, RemoteRootFrom: cfg.rootFrom,
		RemoteMeta: cfg.remoteMeta, RemoteMetaFrom: cfg.metaFrom, ConfigGap: s.settingsModeGap(cfg),
		CoordID: s.prep.coordID, CoordHost: s.prep.coordHost, MetaDir: s.prep.metaDir, SettingsPath: s.prep.settingsPath,
		Net: s.effNetLocked(), Running: s.running, Updated: s.now(), LastAction: s.lastAct,
		IntervalS: int(s.intervalLocked(s.settings) / time.Second),
		Settings:  s.settings, SettingsProblems: s.problems, Notices: s.notices,
		ActiveWorkDir: s.builtWork, ActiveSyncDir: s.builtSync,
		ActiveRemoteRoot: s.builtRemote, ActiveRemoteRootFrom: s.builtRemoteFrom, ActiveHost: s.builtHost,
		SwitchBusy: s.switchBusy != "", SwitchReason: s.switchBusy,
		StartedUp: s.startedUp, Startup: s.startup, StartupError: s.startupEr,
		LastRound: s.lastRound, BuildError: s.buildErr,
	}
	if !s.nextAt.IsZero() {
		n := s.nextAt
		out.NextRun = &n
	}
	if s.sched != nil {
		out.PendingTouched = s.sched.PendingTouched()
	}
	// 生效限速报传输器上实际挂着的值（setNet/每轮都会刷）；还没建传输器时是 0（不限）。
	if tr := s.transfer; tr != nil {
		out.LimitBps = tr.Limit(projsync.PFUpload)
	}
	return out
}

// persist 把状态原子写到状态文件（临时文件 + 改名，0600）；失败只忽略（仅用于诊断）。
func (s *projfilesSvc) persist() {
	if s.prep == nil || s.prep.statusPath == "" {
		return
	}
	b, err := json.MarshalIndent(s.status(), "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.prep.statusPath)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".projfiles-round-*.tmp")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Chmod(0o600)
	clerr := f.Close()
	if werr != nil || cerr != nil || clerr != nil || os.Rename(tmp, s.prep.statusPath) != nil {
		os.Remove(tmp)
	}
}

// Close 干净收尾：调度器与传输器收尾、关掉本服务开的 SFTP 会话。
func (s *projfilesSvc) Close() {
	s.mu.Lock()
	sched, transfer := s.sched, s.transfer
	s.mu.Unlock()
	if sched != nil {
		sched.Close()
	}
	if transfer != nil {
		transfer.Close()
	}
	s.detachSFTP()
}

// detachSFTP 关掉并清空当前 SFTP 会话（整套重建 / 换主机 / 收尾时调）；没挂着会话时空转。
func (s *projfilesSvc) detachSFTP() {
	s.sftpFS.mu.Lock()
	sess := s.sftpFS.sess
	s.sftpFS.sess = nil
	s.sftpFS.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
}
