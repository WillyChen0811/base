//go:build itest

// T14c 第三批：网络场景（S10 限速、S11 断线续传、S7 插队取最新、S8 取最新）。
// 网络现场全部经 itProxy（本机 127.0.0.1 转发到真实 Ubuntu）：限速/掐断/拒绝新连接/字节计数都在代理上做，
// SSH+SFTP 真握手流程不改，主机公钥校验照常。
//
// 关于「limit_kbps_expensive=300」：生产没有这个名字的配置项；对应语义是
// PFSpeedDefaultHotspot=300_000（热点缺省估计速率 300KB/s，用于暂缓判定/测速缺省）和 PFTransfer.SetLimit
// （运行时方向限速，本轮没有走它——暂缓判定是按估计速率算的，物理慢必须造出来）。
// 所以场景把热点档 300KB/s 做成代理的物理限速，验收「热点限速下真实耗时与字节数」。

package itest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// fileBody 造一段确定性的内容（salt 不同则内容不同，用于同一文件名的第二轮修改）。
func fileBody(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + int(salt)*7 + 3)
	}
	return b
}

// ubExists 经 runner（每次新开 SSH 连接，掐断后也能用）问一个路径在不在。
func ubExists(t *testing.T, runner *itRunner, p string) bool {
	t.Helper()
	out, _, _, err := runner.Run(context.Background(), "test -e "+quoteShell(p)+" && echo y || echo n", nil)
	if err != nil {
		t.Fatalf("Ubuntu 探测 %s 失败：%v", p, err)
	}
	return strings.TrimSpace(string(out)) == "y"
}

// ubSize 经 runner 取一个文件的字节数（不存在返回 -1）。
func ubSize(t *testing.T, runner *itRunner, p string) int64 {
	t.Helper()
	out, _, exit, err := runner.Run(context.Background(), "stat -c %s "+quoteShell(p)+" 2>/dev/null || echo -1", nil)
	if err != nil {
		t.Fatalf("Ubuntu stat %s 失败：%v", p, err)
	}
	_ = exit
	var n int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &n); err != nil {
		t.Fatalf("解析 Ubuntu stat 输出 %q 失败：%v", out, err)
	}
	return n
}

// ── S10 热点限速 ────────────────────────────────────────

// S10 限速：网络 expensive + 热点档限速 300KB/s（代理物理限）→ 传 3MB 文件名义 ~10.5 秒，断言 ≥7 秒；
// 代理上行字节与文件大小相符（允许少量协议开销）。改 good 档 + 不限速传同尺寸新内容 → 明显更快（不到慢档一半）。
func TestItestNetS10ExpensiveLimit(t *testing.T) {
	px := newITProxy(t, envReq(t, "PROJFILES_ITEST_ADDR"))
	const mb3 = 3 << 20
	px.SetLimits(300<<10, 0) // 300KB/s，与生产热点缺省 PFSpeedDefaultHotspot=300_000 同档
	c := newClusterAddr(t, px.Addr())
	c.sch.SetNet(projsync.PFRemoteNetExpensive, time.Now())

	name := "热点上传.bin"
	v1 := fileBody(mb3, 7)
	c.startUnit(relA, map[string][]byte{"正文.txt": []byte("正文\n"), name: v1})

	px.ResetCounts()
	t0 := time.Now()
	rep := c.round(true)
	slow := time.Since(t0)
	up := px.Up()
	t.Logf("S10 热点限速轮：用时=%v 代理上行=%d 字节（文件 %d）", slow, up, mb3)
	if slow < 7*time.Second {
		t.Errorf("3MB@300KB/s 名义 ~10.4 秒，实测只用了 %v（应 ≥7 秒）", slow)
	}
	if up < mb3 {
		t.Errorf("代理上行计数 %d < 文件大小 %d：文件没传够", up, mb3)
	}
	if up > mb3+2<<20 {
		t.Errorf("代理上行计数 %d 比文件大小 %d 多出超过 2MB（协议开销失控？）", up, mb3)
	}
	c.assertAligned(relA, rep)

	// good 档 + 不限速：同尺寸新内容，明显更快。
	px.SetLimits(0, 0)
	c.sch.SetNet(projsync.PFRemoteNetGood, time.Now())
	v2 := fileBody(mb3, 11)
	c.writeMacInSync(relA, map[string][]byte{name: v2})
	c.sleepQuiet()
	t1 := time.Now()
	rep2 := c.round(true)
	fast := time.Since(t1)
	t.Logf("S10 good 不限速轮：用时=%v 代理本轮上行=%d 字节", fast, px.Up()-up)
	if fast*2 >= slow {
		t.Errorf("不限速 %v 不够明显快于限速档 %v（要求快一半以上）", fast, slow)
	}
	c.assertAligned(relA, rep2)
}

// ── S11 断线续传 ────────────────────────────────────────

// S11 断线续传：上行限速 2MB/s 传 200MB 大文件，传到约 40% 时代理掐断 →
// 本轮报告该文件没传完（单元 uncertain、动作记 stopped 没执行，不是出错丢弃源；Mac 源完好）；
// Ubuntu 正式位置没有半个文件，暂存区里有 <sha>.part；重连再跑一轮 → 续传完成、两边 sha 一致；
// 代理两轮合计上行字节 < 文件大小 + 2 个块（PFTransferDefaultBlock=8MB，只允许重叠重传掐断时那一块量级）。
func TestItestNetS11ResumeAfterCut(t *testing.T) {
	px := newITProxy(t, envReq(t, "PROJFILES_ITEST_ADDR"))
	const mb200 = 200 << 20
	px.SetLimits(2<<20, 0)
	c := newClusterAddr(t, px.Addr())

	name := "母带.bin"
	content := fileBody(mb200, 3)
	wantSHA := sha256Hex(content)
	c.startUnit(relA, map[string][]byte{name: content, "说明.txt": []byte("说明\n")})
	px.ResetCounts()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	done := make(chan *projsync.PFSchedReport, 1)
	go func() { done <- c.sch.RunRound(ctx, projsync.PFSchedRoundOptions{Full: true}) }()

	// 传到约一半（≥40%）掐断；兜底：若 8 分钟还没到 40% 也强制掐（随后断言会发现没到而失败）。
	cutAt := int64(mb200) * 2 / 5
	cutFired := false
	deadline := time.Now().Add(8 * time.Minute)
	for !cutFired && time.Now().Before(deadline) {
		if px.Up() >= cutAt {
			px.Cut()
			cutFired = true
		} else {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !cutFired {
		t.Fatalf("上传一直没到 %d 字节就无法按剧本掐断：up=%d", int64(cutAt), px.Up())
	}
	rep1 := <-done
	upR1 := px.Up()
	t.Logf("S11 掐断轮：上行=%d / 文件=%d，报告=%s", upR1, mb200, jsonOf(rep1))

	res1 := unitResult(rep1, codeA)
	if res1 == nil {
		t.Fatalf("掐断轮报告里没有单元 %s：%s", codeA, jsonOf(rep1.Units))
	}
	if !res1.Uncertain {
		t.Errorf("掐断后单元应该是 uncertain（通信不确定、停后续写）：%s", jsonOf(res1))
	}
	if !strings.Contains(jsonOf(res1), name) {
		t.Errorf("报告对这个大文件没有交代：%s", jsonOf(res1))
	}
	// 不是「出错丢弃源」：Mac 源文件内容完好。
	if got := c.macFilesOf(relA)[name]; got != wantSHA {
		t.Errorf("Mac 源文件被弄坏了：sha=%s 应为 %s", got, wantSHA)
	}
	// Ubuntu 正式位置不能出现半个文件（重连前用 runner 查，SFTP 长连接已被掐）。
	ubFinal := c.env.root + "/" + relA + "/" + name
	if ubExists(t, c.env.runner, ubFinal) {
		t.Errorf("掐断后 Ubuntu 正式位置出现了文件：%s", ubFinal)
	}
	part := projsync.PFRemoteStagingDir(c.env.meta) + "/" + wantSHA + ".part"
	partSize := ubSize(t, c.env.runner, part)
	if partSize <= 0 {
		t.Errorf("Ubuntu 暂存区里没有续传 part（%s size=%d）", part, partSize)
	}
	if partSize >= mb200 {
		t.Errorf("续传 part 竟然已经完整（size=%d），那第二轮就不是续传了", partSize)
	}
	t.Logf("S11 掐断现场：part=%s size=%d（文件 %d）", part, partSize, mb200)

	// 重连 → 第二轮续传完成。
	c.redialRemote()
	rep2 := c.round(true)
	upTotal := px.Up()
	t.Logf("S11 续传轮完成：两轮合计上行=%d（文件=%d 块=%d 上限=%d）", upTotal, mb200,
		projsync.PFTransferDefaultBlock, mb200+2*int64(projsync.PFTransferDefaultBlock))
	res2 := unitResult(rep2, codeA)
	if res2 == nil {
		t.Fatalf("续传轮没有单元结果：%s", jsonOf(rep2.Units))
	}
	if got := c.macFilesOf(relA)[name]; got != wantSHA {
		t.Errorf("续传后 Mac 内容变了：sha=%s", got)
	}
	if got := c.ubFilesOf(relA)[name]; got != wantSHA {
		t.Errorf("续传后 Ubuntu 内容不对：sha=%s 应为 %s", got, wantSHA)
	}
	if upTotal < mb200 {
		t.Errorf("两轮合计上行 %d < 文件大小 %d：字节没传够", upTotal, mb200)
	}
	if upTotal >= int64(mb200)+2*int64(projsync.PFTransferDefaultBlock) {
		t.Errorf("两轮合计上行 %d ≥ 文件大小+2块（%d）：续传没省下单块级重传", upTotal,
			int64(mb200)+2*int64(projsync.PFTransferDefaultBlock))
	}
	c.assertAligned(relA, rep2)
}

// ── S7 插队 ─────────────────────────────────────────────

// S7 插队：上行限速 400KB/s，先在后台起一轮把 40MB 大文件排上传（~100 秒）；
// 等大文件真的在传（代理上行 ≥8MB）后，对另一个项目在 Ubuntu 上新写的文件调取最新
// （PFFresh.Paths，用户优先级 100）→ 小文件几秒内先到 Mac，此时大文件还没上传完（Ubuntu 正式位置没有它）。
// 等慢轮收尾后两边终归一致。
func TestItestNetS7FreshPreemptsBigUpload(t *testing.T) {
	px := newITProxy(t, envReq(t, "PROJFILES_ITEST_ADDR"))
	const mb40 = 40 << 20
	px.SetLimits(400<<10, 0) // 只慢上行；下行（取最新）不限
	c := newClusterAddr(t, px.Addr())
	fresh, err := projsync.NewPFFresh(c.sch, projsync.PFFreshConfig{SyncDir: c.sync})
	if err != nil {
		t.Fatalf("建 PFFresh 失败：%v", err)
	}

	bigName := "大素材.bin"
	c.startUnit(relA, map[string][]byte{bigName: fileBody(mb40, 5), "说明.txt": []byte("说明\n")})
	c.startUnit(relB, map[string][]byte{"备忘.txt": []byte("备忘\n")})

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	done := make(chan *projsync.PFSchedReport, 1)
	t0 := time.Now()
	go func() { done <- c.sch.RunRound(ctx, projsync.PFSchedRoundOptions{Full: true}) }()

	// 确认大文件在传（≥8MB 上行）；若 3 分钟没到，测试直接失败（报告已在 done 里）。
	inFlight := false
	deadline := time.Now().Add(3 * time.Minute)
	for !inFlight && time.Now().Before(deadline) {
		if px.Up() >= 8<<20 {
			inFlight = true
		} else {
			select {
			case rep := <-done:
				t.Fatalf("大文件上传没开始（up=%d）轮就回来了：%s", px.Up(), jsonOf(rep))
			default:
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !inFlight {
		t.Fatalf("等待大文件上传超时（up=%d）", px.Up())
	}

	// 对另一个项目的小文件插队取最新。
	smallName := "急用.txt"
	ubV := []byte("Ubuntu 上刚写好的急用内容\n")
	c.writeUb(relB, map[string][]byte{smallName: ubV})
	c.sleepQuiet()
	px.ResetCounts()
	f0 := time.Now()
	fres := fresh.Paths(ctx, []string{filepathJoin(c.work, relB, smallName)}, 60*time.Second)
	freshDur := time.Since(f0)
	if len(fres.Items) != 1 {
		t.Fatalf("取最新返回条目数不对：%s", jsonOf(fres))
	}
	it := fres.Items[0]
	t.Logf("S7 取最新：state=%s 用时=%v note=%s", it.State, freshDur, fres.Note)
	if it.State != projsync.PFFreshStateFetched {
		t.Errorf("取最新状态应为「已取最新」，实际 %q（note=%s）", it.State, fres.Note)
	}
	if got := c.macFilesOf(relB)[smallName]; got != sha256Hex(ubV) {
		t.Errorf("Mac 本地内容没更新到 Ubuntu 版本：sha=%s", got)
	}
	// 插队成立：此刻大文件还没落到 Ubuntu 正式位置。
	if ubExists(t, c.env.runner, c.env.root+"/"+relA+"/"+bigName) {
		t.Errorf("取最新返回时大文件已装到 Ubuntu——没有形成「小文件先到」的现场")
	}
	t.Logf("S7 现场：取最新 %v 完成时上行=%d（大文件 %d），fresh 轮下行=%d", freshDur, px.Up(), mb40, fres.Items[0].Size)

	rep := <-done
	bigDur := time.Since(t0)
	t.Logf("S7 慢轮收尾：用时=%v up=%d down=%d", bigDur, px.Up(), px.Down())
	if bigDur <= freshDur {
		t.Errorf("慢轮（%v）不比取最新（%v）长，插队现场不成立", bigDur, freshDur)
	}
	c.assertAligned(relA, rep)
	c.assertAligned(relB, rep)
}

// ── S8 取最新 ───────────────────────────────────────────

// S8 取最新：Ubuntu 改一个文件 → Mac 调 PFFresh.Paths 取它 → 状态「已取最新」、Mac 本地内容已更新；
// Ubuntu 断开（代理拒绝新连接 + 掐断在途）→ 再调取最新 → 答「对方不在线」或「未能确认」，不报错、不卡过预算。
func TestItestNetS8FreshPaths(t *testing.T) {
	px := newITProxy(t, envReq(t, "PROJFILES_ITEST_ADDR"))
	c := newClusterAddr(t, px.Addr())
	fresh, err := projsync.NewPFFresh(c.sch, projsync.PFFreshConfig{SyncDir: c.sync})
	if err != nil {
		t.Fatalf("建 PFFresh 失败：%v", err)
	}

	name := "稿子.txt"
	c.startUnit(relA, map[string][]byte{name: []byte("旧内容\n"), "陪衬.txt": []byte("陪衬\n")})
	rep := c.round(true)
	c.assertAligned(relA, rep)

	// Ubuntu 改 → Mac 取最新。
	ubV := []byte("Ubuntu 上改过的新内容\n")
	c.writeUb(relA, map[string][]byte{name: ubV})
	c.sleepQuiet()
	desktop := filepathJoin(c.work, relA, name)
	f0 := time.Now()
	fres := fresh.Paths(context.Background(), []string{desktop}, 30*time.Second)
	t.Logf("S8 在线取最新：state=%s 用时=%v note=%s", fres.Items[0].State, time.Since(f0), fres.Note)
	if fres.Items[0].State != projsync.PFFreshStateFetched {
		t.Errorf("Ubuntu 改过之后取最新应「已取最新」，实际 %q（note=%s）", fres.Items[0].State, fres.Note)
	}
	b, err := os.ReadFile(filepathJoin(c.sync, relA, name))
	if err != nil {
		t.Fatalf("读 Mac 同步目录失败：%v", err)
	}
	if string(b) != string(ubV) {
		t.Errorf("Mac 本地内容没更新：得到 %q", b)
	}

	// Ubuntu 断开：拒绝新连接 + 掐断在途 → 取最新如实答离线/未能确认，不报错、不卡预算。
	px.SetReject(true)
	px.Cut()
	budget := 8 * time.Second
	f1 := time.Now()
	fres2 := fresh.Paths(context.Background(), []string{desktop}, budget)
	el := time.Since(f1)
	st := fres2.Items[0].State
	t.Logf("S8 离线取最新：state=%s 用时=%v note=%s", st, el, fres2.Note)
	if st != projsync.PFFreshStateOffline && st != projsync.PFFreshStateUnknown {
		t.Errorf("Ubuntu 断开时取最新应答「对方不在线」或「未能确认」，实际 %q（note=%s）", st, fres2.Note)
	}
	if el >= budget {
		t.Errorf("离线取最新卡过了预算：用时 %v ≥ 预算 %v", el, budget)
	}
	if fres2.Items[0].Path != desktop {
		t.Errorf("结果路径没原样带回：%s", jsonOf(fres2))
	}

	// 收尾：恢复网络，确认这份内容回到一致（也验证恢复路径）。
	px.SetReject(false)
	c.redialRemote()
	c.sleepQuiet()
	rep2 := c.round(true)
	c.assertAligned(relA, rep2)
}
