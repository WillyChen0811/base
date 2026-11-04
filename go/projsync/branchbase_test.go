package projsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tOID1, tOID2 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"

func TestBranchBaseMissingThenPersistAcrossLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "base")
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	b, notes, err := loadBranchBase(dir, "proj", "/m/.git", "/u/repo", now)
	if err != nil || len(notes) != 0 || b == nil || len(b.names()) != 0 {
		t.Fatalf("没有文件应得到空基线: %v %v %+v", err, notes, b)
	}
	if err := b.save(now); err != nil { // 首次：即使没有分支也写出文件
		t.Fatal(err)
	}
	if _, err := os.Stat(b.path); err != nil {
		t.Fatal("首次保存后应有基线文件")
	}
	b.set("f", tOID1)
	b.set("g", tOID2)
	if err := b.save(now); err != nil {
		t.Fatal(err)
	}
	// 「重启进程」：从磁盘重新加载。
	b2, _, err := loadBranchBase(dir, "proj", "/m/.git", "/u/repo", now)
	if err != nil || b2.get("f") != tOID1 || b2.get("g") != tOID2 {
		t.Fatalf("重新加载后基线不对: %v %+v", err, b2)
	}
	b2.del("f")
	if err := b2.save(now); err != nil {
		t.Fatal(err)
	}
	b3, _, _ := loadBranchBase(dir, "proj", "/m/.git", "/u/repo", now)
	if b3.get("f") != "" || b3.get("g") != tOID2 {
		t.Fatalf("删除没落盘: %+v", b3.file.Branches)
	}
	// 另一对仓库是另一份基线。
	other, _, _ := loadBranchBase(dir, "proj", "/m/.git", "/u/other", now)
	if other.get("g") != "" {
		t.Fatal("不同仓库对不应共用基线")
	}
	if fi, _ := os.Stat(b.path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限 %v", fi.Mode().Perm())
	}
}

func TestBranchBaseCorruptIsQuarantined(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	for name, content := range map[string]string{
		"坏 JSON":  "{",
		"版本不认识":   `{"version":9,"repo":"/m/.git","remote_repo":"/u/repo","branches":{}}`,
		"仓库路径对不上": `{"version":1,"repo":"/x/.git","remote_repo":"/u/repo","branches":{}}`,
		"OID 不合法": `{"version":1,"repo":"/m/.git","remote_repo":"/u/repo","branches":{"f":"zz"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, branchBaseKey("/m/.git", "/u/repo")+".json")
			os.WriteFile(path, []byte(content), 0o600)
			b, notes, err := loadBranchBase(dir, "proj", "/m/.git", "/u/repo", now)
			if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "只建不删") || len(b.names()) != 0 {
				t.Fatalf("%v %v", err, notes)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("损坏的文件应被移走")
			}
			if ents, _ := os.ReadDir(filepath.Join(dir, pfBadDirName)); len(ents) != 1 {
				t.Fatalf("坏/ 下应有一份原文件: %v", ents)
			}
		})
	}
}

func TestBranchBaseUnreadableIsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, branchBaseKey("/m/.git", "/u/repo")+".json")
	os.WriteFile(path, []byte(`{}`), 0o000)
	if _, _, err := loadBranchBase(dir, "proj", "/m/.git", "/u/repo", time.Now()); err == nil {
		t.Fatal("读不了必须报错，不能当成没有基线")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("读不了时什么都不能动")
	}
}

func TestBranchBaseNilIsNoop(t *testing.T) {
	b, notes, err := loadBranchBase("", "p", "/m", "/u", time.Now())
	if b != nil || notes != nil || err != nil {
		t.Fatal("没配置目录应返回 nil")
	}
	b.set("f", tOID1)
	b.del("f")
	if b.get("f") != "" || b.save(time.Now()) != nil || b.logDelete(branchDeleteRecord{}) == nil {
		t.Fatal("nil 基线：读空、写空转、删除记录报错（于是不会删）")
	}
}
