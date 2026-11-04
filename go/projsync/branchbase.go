package projsync

// 分支对账基线（用户 11/04 定，PLAN-sync-v2.md 第十九节）：每个项目一份，记下每个分支「上一轮结束时两边一致的那个提交」。
// 判定层拿它区分「一边删了」和「另一边新建了」（decidedel.go）。
//
// 存放位置照文件同步的 base（pfbase.go）：只在 Mac 存一份权威副本，放在调用方给的目录下（wgtund 传 ~/.xxyi/projsync/base/）：
//
//	<dir>/<键>.json            基线：整份重写，原子写（临时文件 + fsync + 改名 + fsync 目录）
//	<dir>/<键>.deleted.jsonl   删除记录：每次自动删除前后各追加一行（分支、哪一边、原提交），追加即 fsync，方便找回
//	<dir>/坏/<键>-<时间>.json   内容损坏被移走的原文件
//
// 理由：基线是 Mac 发起的编排（RunRound）自己的状态，和文件同步 base 一样只由 Mac 一个写者维护，不必两边各存一份再对账；
// 放在仓库之外（不写进 .git），不受 git gc / 用户手动操作引用的影响，也不会被交换引用带到对方。
//
// 键 = sha256(Mac 的 git common dir + NUL + Ubuntu 仓库路径) 的前 16 位十六进制；文件里另存这两个路径并在加载时核对，
// 仓库对换了（路径变了）就是另一份基线，不会拿 A 仓库的基线去判 B 仓库。
//
// 「识别边界、超出就停」：读不了（权限、IO）→ 返回 error，本轮不写；内容损坏（JSON 坏、版本不认识、路径对不上、OID 不合法）
// → 原文件移进 坏/，按「没有基线」处理（只会建、不会删，是安全方向），原因写进 Notes。
// 并发：同一仓库同一时间只有一轮（RunRound 持仓库锁），所以单写者，不跨进程加锁。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const branchBaseVersion = 1

type branchBaseFile struct {
	Version    int               `json:"version"`
	Project    string            `json:"project"`     // 只用于人看
	Repo       string            `json:"repo"`        // Mac 仓库的 git common dir（规范化）
	RemoteRepo string            `json:"remote_repo"` // Ubuntu 仓库路径
	Updated    time.Time         `json:"updated"`
	Branches   map[string]string `json:"branches"` // 分支名 → 上一轮两边一致的提交
}

// branchBase 是加载到内存的基线。nil 表示没配置基线目录：get 永远返回空、写操作全部空转（等同旧行为：只建不删）。
type branchBase struct {
	path, logPath string
	file          branchBaseFile
	dirty         bool
}

// branchBaseKey 返回基线文件的键。
func branchBaseKey(repoKey, remoteRepo string) string {
	h := sha256.Sum256([]byte(repoKey + "\x00" + filepath.Clean(remoteRepo)))
	return hex.EncodeToString(h[:8])
}

// loadBranchBase 加载基线；文件不存在时返回空基线（首次：本轮两边相同的分支会被记成初始基线）。
// notes 是损坏时移走文件的说明。dir 为空返回 (nil, nil, nil)。
func loadBranchBase(dir, project, repoKey, remoteRepo string, now time.Time) (*branchBase, []string, error) {
	if dir == "" {
		return nil, nil, nil
	}
	key := branchBaseKey(repoKey, remoteRepo)
	b := &branchBase{path: filepath.Join(dir, key+".json"), logPath: filepath.Join(dir, key+".deleted.jsonl"),
		file: branchBaseFile{Version: branchBaseVersion, Project: project, Repo: repoKey,
			RemoteRepo: filepath.Clean(remoteRepo), Branches: map[string]string{}}}
	raw, err := os.ReadFile(b.path)
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("读取分支基线 %s 失败: %w", b.path, err)
	}
	var f branchBaseFile
	reason := ""
	switch err := json.Unmarshal(raw, &f); {
	case err != nil:
		reason = "JSON 不合法: " + err.Error()
	case f.Version != branchBaseVersion:
		reason = fmt.Sprintf("版本 %d 不认识", f.Version)
	case f.Repo != b.file.Repo || f.RemoteRepo != b.file.RemoteRepo:
		reason = fmt.Sprintf("仓库路径对不上（文件里 %q / %q）", f.Repo, f.RemoteRepo)
	default:
		for name, oid := range f.Branches {
			if name == "" || !oidRe.MatchString(oid) {
				reason = fmt.Sprintf("分支 %q 的提交 %q 不合法", name, oid)
				break
			}
		}
	}
	if reason == "" {
		if f.Branches == nil {
			f.Branches = map[string]string{}
		}
		f.Project = project
		b.file = f
		return b, nil, nil
	}
	moved, err := quarantineBranchBase(dir, key, b.path, now)
	if err != nil {
		return nil, nil, fmt.Errorf("分支基线 %s 已损坏（%s），移走失败: %w", b.path, reason, err)
	}
	b.dirty = true
	return b, []string{fmt.Sprintf("分支对账基线已损坏（%s），原文件移到 %s，本轮按没有基线处理（只建不删）", reason, moved)}, nil
}

func quarantineBranchBase(dir, key, path string, now time.Time) (string, error) {
	bad := filepath.Join(dir, pfBadDirName)
	if err := pfMkdirAllSync(bad, pfDirPerm); err != nil {
		return "", err
	}
	stamp := now.UTC().Format(pfStampLayout)
	dst := filepath.Join(bad, key+"-"+stamp+".json")
	for n := 2; ; n++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dst = filepath.Join(bad, fmt.Sprintf("%s-%s-%d.json", key, stamp, n))
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	if err := pfSyncDir(dir); err != nil {
		return "", err
	}
	return dst, pfSyncDir(bad)
}

// get 返回分支的基线；没有基线返回空串。
func (b *branchBase) get(name string) string {
	if b == nil {
		return ""
	}
	return b.file.Branches[name]
}

func (b *branchBase) set(name, oid string) {
	if b == nil || !oidRe.MatchString(oid) || b.file.Branches[name] == oid {
		return
	}
	b.file.Branches[name] = oid
	b.dirty = true
}

func (b *branchBase) del(name string) {
	if b == nil {
		return
	}
	if _, ok := b.file.Branches[name]; ok {
		delete(b.file.Branches, name)
		b.dirty = true
	}
}

// names 返回基线里所有分支名（排序）。
func (b *branchBase) names() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.file.Branches))
	for n := range b.file.Branches {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// save 把基线原子写回磁盘。文件还不存在时即使没有变化也写一次（首次初始化后就有文件了）。
func (b *branchBase) save(now time.Time) error {
	if b == nil {
		return nil
	}
	if !b.dirty {
		if _, err := os.Lstat(b.path); err == nil {
			return nil
		}
	}
	b.file.Updated = now.UTC()
	data, err := json.MarshalIndent(b.file, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(b.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写分支基线 %s 失败: %w", b.path, err)
	}
	b.dirty = false
	return nil
}

// branchDeleteRecord 是删除记录的一行。State：deleting（删之前写）、deleted / failed / uncertain（删之后写）。
type branchDeleteRecord struct {
	At      time.Time `json:"at"`
	Project string    `json:"project"`
	Branch  string    `json:"branch"`
	Side    Side      `json:"side"`
	OID     string    `json:"oid"`
	State   string    `json:"state"`
	Detail  string    `json:"detail,omitempty"`
}

// logDelete 往删除记录追加一行并 fsync。
func (b *branchBase) logDelete(rec branchDeleteRecord) error {
	if b == nil {
		return errors.New("没有配置分支基线目录")
	}
	rec.Project = b.file.Project
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := pfMkdirAllSync(filepath.Dir(b.logPath), pfDirPerm); err != nil {
		return err
	}
	return appendLineSync(b.logPath, line, 0o600)
}
