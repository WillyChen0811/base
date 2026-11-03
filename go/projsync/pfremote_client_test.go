package projsync

// T8 客户端测试：通信失败 = 结果不确定（不重试）、响应的字段组合校验、本机校验、对方明确拒绝与通信失败要分清。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// pfrtMutate 返回一个改响应的函数：把响应解码成 T、交给 f 改、再编码回去。
func pfrtMutate[T any](sub string, f func(*T)) func(string, []byte) []byte {
	return func(s string, out []byte) []byte {
		if s != sub {
			return out
		}
		var v T
		if err := json.Unmarshal(out, &v); err != nil {
			panic(err)
		}
		f(&v)
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return b
	}
}

func TestPFRemoteClientCommFailureIsUncertain(t *testing.T) {
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	ctx := context.Background()
	calls := map[string]func() error{
		"poll": func() error { _, err := f.cl.Poll(ctx, PFRemotePollOptions{}); return err },
		"facts": func() error {
			_, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
			return err
		},
		"apply": func() error {
			_, err := f.apply(PFRemoteStep{Op: PFRemoteOpMkdir, Path: "新"})
			return err
		},
	}
	setups := map[string]func(){
		"连接层失败": func() { f.run.fail = errors.New("ssh: 连接断了") },
		"退出码非0": func() { f.run.exit = 3 },
		"响应不是JSON": func() {
			f.run.mutate = func(string, []byte) []byte { return []byte("this is not json") }
		},
		"响应是空的": func() { f.run.mutate = func(string, []byte) []byte { return nil } },
		"响应版本不对": func() {
			f.run.mutate = func(_ string, out []byte) []byte {
				return []byte(strings.Replace(string(out), `"version":1`, `"version":9`, 1))
			}
		},
	}
	for sname, setup := range setups {
		for cname, call := range calls {
			f.run.fail, f.run.exit, f.run.mutate = nil, 0, nil
			setup()
			before := f.run.calls
			err := call()
			if !PFRemoteIsUncertain(err) || !errors.Is(err, ErrPFRemoteUncertain) {
				t.Errorf("%s / %s: 应是「结果不确定」: %v", sname, cname, err)
			}
			var re *PFRemoteError
			if !errors.As(err, &re) || re.Kind != PFRemoteErrComm {
				t.Errorf("%s / %s: 应是 *PFRemoteError 的通信类: %v", sname, cname, err)
			}
			if f.run.calls != before+1 {
				t.Errorf("%s / %s: 不该重试，调用了 %d 次", sname, cname, f.run.calls-before)
			}
		}
	}
	f.run.fail, f.run.exit, f.run.mutate = nil, 0, nil
	// 通信类错误里要带着对方 stderr 的原因，方便排查
	f.run.exit = 3
	if _, err := f.cl.Poll(ctx, PFRemotePollOptions{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("错误里应带 stderr: %v", err)
	}
}

func TestPFRemoteClientApplyResponseChecks(t *testing.T) {
	ctx := context.Background()
	steps := func(f *pfrtFix) []PFRemoteStep {
		sha, size := f.stage("内容")
		return []PFRemoteStep{
			{Op: PFRemoteOpMkdir, Path: "新"},
			{Op: PFRemoteOpInstallNew, Path: "新/n.txt", SHA256: sha, Size: size, MtimeNs: pfrtMt},
		}
	}
	mut := map[string]func(*PFRemoteApplyResponse){
		"少了一项结果却没说停":       func(r *PFRemoteApplyResponse) { r.Results = r.Results[:1] },
		"多出一项结果":           func(r *PFRemoteApplyResponse) { r.Results = append(r.Results, r.Results[0]) },
		"结果种类对不上步骤":        func(r *PFRemoteApplyResponse) { r.Results[0].Op = PFRemoteOpRmdir },
		"安装完成但哈希不是 part 的": func(r *PFRemoteApplyResponse) { r.Results[1].Apply.Installed.SHA256 = pfrtSHA("别的") },
		"安装完成但没有安装结果":      func(r *PFRemoteApplyResponse) { r.Results[1].Apply.Installed = nil },
		"标了停止但位置不对":        func(r *PFRemoteApplyResponse) { r.Stopped, r.StopAt, r.StopReason = true, 5, "x" },
		"标了停止但没说原因":        func(r *PFRemoteApplyResponse) { r.Stopped, r.StopAt = true, 1 },
		"没停却带停止位置":         func(r *PFRemoteApplyResponse) { r.StopAt = 1 },
		"完成却带不确定":          func(r *PFRemoteApplyResponse) { r.Results[0].Apply.Uncertain = true },
		"完成的原因代码不是 ok":     func(r *PFRemoteApplyResponse) { r.Results[0].Apply.Code = "target_exists" },
		"没完成却没有原因代码": func(r *PFRemoteApplyResponse) {
			r.Results[0].Apply.Status, r.Results[0].Apply.Code = PFApplyAbandoned, ""
		},
		"推迟却标不确定": func(r *PFRemoteApplyResponse) {
			a := r.Results[0].Apply
			a.Status, a.Code, a.Uncertain = PFApplyDeferred, "icloud", true
		},
		"缺少写原语的结果":   func(r *PFRemoteApplyResponse) { r.Results[0].Apply = nil },
		"出错的步骤还带着结果": func(r *PFRemoteApplyResponse) { r.Results[0].Error = "x" },
		"配置错误没说原因":   func(r *PFRemoteApplyResponse) { r.Results[0].Config = true },
		"整批拒绝却带结果":   func(r *PFRemoteApplyResponse) { r.Error = "拒绝" },
	}
	for name, m := range mut {
		f := newPFRTFix(t)
		f.put("a.txt", "A")
		st := steps(f)
		f.run.mutate = pfrtMutate(PFRemoteSubApply, m)
		_, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: f.unit()}, st)
		if !PFRemoteIsUncertain(err) {
			t.Errorf("%s: 响应不合法应按「结果不确定」处理: %v", name, err)
		}
		if !f.exists("新/n.txt") {
			// 服务端确实执行了——正因为这样才必须按不确定处理
			t.Errorf("%s: 夹具问题，服务端应已执行", name)
		}
	}
	// 对方整批拒绝（没带结果）：不是不确定
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	f.run.mutate = pfrtMutate(PFRemoteSubApply, func(r *PFRemoteApplyResponse) { r.Error, r.Results = "对方说不做", nil })
	_, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: f.unit()}, steps(f))
	var re *PFRemoteError
	if !errors.As(err, &re) || re.Kind != PFRemoteErrRefused || PFRemoteIsUncertain(err) || !strings.Contains(re.Msg, "对方说不做") {
		t.Fatalf("%v", err)
	}
}

func TestPFRemoteClientPollResponseChecks(t *testing.T) {
	ctx := context.Background()
	mut := map[string]func(*PFRemotePollResponse){
		"授予的租约不是我们的": func(r *PFRemotePollResponse) { r.Lease.Holder.ID = "mac-X" },
		"没授予也没说原因":   func(r *PFRemotePollResponse) { r.Lease.Granted, r.Lease.Reason = false, "" },
		"缺 units":    func(r *PFRemotePollResponse) { r.Units = nil },
		"缺 requests": func(r *PFRemotePollResponse) { r.Requests = nil },
		"指纹格式不对":     func(r *PFRemotePollResponse) { r.Units[0].Fingerprint = "xyz" },
		"没指纹也没原因":    func(r *PFRemotePollResponse) { r.Units[0].Fingerprint = "" },
		"单元路径重复":     func(r *PFRemotePollResponse) { r.Units = append(r.Units, r.Units[0]) },
		"单元键不合法":     func(r *PFRemotePollResponse) { r.Units[0].Code = "小写" },
		"标了重复却没对方路径": func(r *PFRemotePollResponse) { r.Units[0].Dup = true },
		"请求不合法":      func(r *PFRemotePollResponse) { r.Requests[0].Request.Priority = "root" },
	}
	for name, m := range mut {
		f := newPFRTFix(t)
		f.put("a.txt", "A")
		_, err := PFRemoteWriteRequest(f.meta, PFRemoteRequest{Kind: PFRemoteReqTouched, Unit: pfrtCode, Priority: PFRemotePrioUser}, f.now)
		pfrtNoErr(t, err)
		f.run.mutate = pfrtMutate(PFRemoteSubPoll, m)
		_, err = f.cl.Poll(ctx, PFRemotePollOptions{})
		if !PFRemoteIsUncertain(err) {
			t.Errorf("%s: 应按不确定处理: %v", name, err)
		}
	}
}

func TestPFRemoteClientFactsResponseChecks(t *testing.T) {
	ctx := context.Background()
	mut := map[string]func(*PFRemoteFactsResponse){
		"指纹格式不对":      func(r *PFRemoteFactsResponse) { r.Fingerprint = "xyz" },
		"条目种类不认识":     func(r *PFRemoteFactsResponse) { r.Items[0].Kind = "z" },
		"条目顺序错":       func(r *PFRemoteFactsResponse) { r.Items[0], r.Items[1] = r.Items[1], r.Items[0] },
		"条目重复":        func(r *PFRemoteFactsResponse) { r.Items[1].Path = r.Items[0].Path },
		"缺单元路径":       func(r *PFRemoteFactsResponse) { r.UnitPath = "" },
		"标了重复却没有对方路径": func(r *PFRemoteFactsResponse) { r.Dup = true },
		"游标没前进":       func(r *PFRemoteFactsResponse) { r.More = true },
		"单元不存在却带内容":   func(r *PFRemoteFactsResponse) { r.Exists = false },
	}
	for name, m := range mut {
		f := newPFRTFix(t)
		f.put("a.txt", "A")
		f.put("b.txt", "B")
		f.run.mutate = pfrtMutate(PFRemoteSubFacts, m)
		_, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
		if !PFRemoteIsUncertain(err) {
			t.Errorf("%s: 应按不确定处理: %v", name, err)
		}
	}
	// 条目内容被改（传输出错）：用收到的条目重算的指纹对不上 → 对不上（没写过东西，不是不确定）
	f := newPFRTFix(t)
	f.put("a.txt", "A")
	f.run.mutate = pfrtMutate(PFRemoteSubFacts, func(r *PFRemoteFactsResponse) { r.Items[0].Size++ })
	_, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
	if !errors.Is(err, ErrPFRemoteUnstable) || PFRemoteIsUncertain(err) {
		t.Fatalf("重算的指纹对不上应报 Unstable: %v", err)
	}
	// 路径里有转义（原路径不是合法 UTF-8）时不重算指纹，但其余照常校验
	f = newPFRTFix(t)
	f.put("a.txt", "A")
	f.run.mutate = pfrtMutate(PFRemoteSubFacts, func(r *PFRemoteFactsResponse) { r.Items[0].Path, r.Escaped = `a\xff.txt`, 1 })
	facts, err := f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{})
	pfrtNoErr(t, err)
	if facts.Scan.Items[0].Path != `a\xff.txt` {
		t.Fatalf("%+v", facts.Scan.Items[0])
	}
}

func TestPFRemoteClientLocalChecks(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	local := func(name string, err error) {
		t.Helper()
		var re *PFRemoteError
		if !errors.As(err, &re) || re.Kind != PFRemoteErrLocal || PFRemoteIsUncertain(err) {
			t.Errorf("%s: 应在本机拦住: %v", name, err)
		}
	}
	// 请求太大：不发
	keep := make([]string, 70000)
	for i := range keep {
		keep[i] = pfrtSHA("k")
	}
	_, err := f.cl.Apply(ctx, PFRemoteApplyOptions{}, []PFRemoteStep{
		{Op: PFRemoteOpStagingClean, Keep: keep}, {Op: PFRemoteOpStagingClean, Keep: keep},
	})
	local("请求太大", err)
	if f.run.calls != 0 || !strings.Contains(err.Error(), "分批") {
		t.Fatalf("太大的请求不该发出去: calls=%d %v", f.run.calls, err)
	}
	// peerctl 路径不合规：不发
	c := *f.cl
	c.RemoteCmd = "/usr/bin/x; rm -rf /"
	_, err = c.Poll(ctx, PFRemotePollOptions{})
	local("路径不合规", err)
	// 没有 Runner
	c = *f.cl
	c.Runner = nil
	_, err = c.Poll(ctx, PFRemotePollOptions{})
	local("没有 Runner", err)
	// facts 的参数
	_, err = f.cl.Facts(ctx, PFRemoteUnit{Code: "小写", Rel: "a"}, PFRemoteFactsOptions{})
	local("单元键不合法", err)
	_, err = f.cl.Facts(ctx, f.unit(), PFRemoteFactsOptions{MaxItems: 99999})
	local("条目上限只能调小", err)
	if f.run.calls != 0 {
		t.Fatalf("本机拦住的都不该发出去: %d", f.run.calls)
	}
}

// 对方说「不做」（拒绝）和通信失败要分得清：前者什么都没写，后者结果不确定。
func TestPFRemoteClientRefusedIsNotUncertain(t *testing.T) {
	f := newPFRTFix(t)
	ctx := context.Background()
	_, err := f.cl.Apply(ctx, PFRemoteApplyOptions{Unit: PFRemoteUnit{Code: "260403J9999", Rel: "2026/260403J9999-无"}}, []PFRemoteStep{{Op: PFRemoteOpMkdir, Path: "x"}})
	var re *PFRemoteError
	if !errors.As(err, &re) || re.Kind != PFRemoteErrRefused || PFRemoteIsUncertain(err) || errors.Is(err, ErrPFRemoteLeaseHeld) {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(re.Error(), "projfiles-apply") {
		t.Fatalf("错误里应带子命令名: %v", re)
	}
}
