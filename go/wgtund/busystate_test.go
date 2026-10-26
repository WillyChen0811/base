package wgtund

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

// execWithBusy 在假 tmux 外面再接一个"读状态目录"的命令：命中 busyReadCommand 就回 out / 失败码，并计数。
func execWithBusy(f *fakeTmux, out, stderr string, code int, calls *int32) func(string) (string, string, int) {
	return func(cmd string) (string, string, int) {
		if cmd == busyReadCommand() {
			atomic.AddInt32(calls, 1)
			return out, stderr, code
		}
		return f.exec(cmd)
	}
}

type busyItem struct {
	Name     string `json:"name"`
	Busy     string `json:"busy"`
	BusyTime int64  `json:"busy_time"`
}

// getSessionsRaw 取 /sessions 的原始 JSON 和解析后的项。
func getSessionsRaw(t *testing.T, st started) (raw []byte, items []busyItem, rawItems []map[string]any) {
	t.Helper()
	resp, b := call(t, st, "GET", "/sessions?host=ubuntu")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /sessions 应 200，得到 %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Sessions []busyItem `json:"sessions"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("解析失败 %v: %s", err, b)
	}
	var out2 struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(b, &out2); err != nil {
		t.Fatal(err)
	}
	return b, out.Sessions, out2.Sessions
}

func newSessions(t *testing.T, st started, names ...string) {
	t.Helper()
	for _, n := range names {
		if resp := newSession(t, st, n); resp.StatusCode != 200 {
			t.Fatalf("新建 %s 应 200，得到 %d", n, resp.StatusCode)
		}
	}
}

// 有状态文件的会话挂 busy/busy_time；坏文件、没文件的会话是空串和 0；会话没了残留的文件不出现。
func TestSessionsBusyState(t *testing.T) {
	f := newFakeTmux()
	var calls int32
	statesOut := strings.Join([]string{
		`alpha.json` + "\t" + `{"state":"busy","time":1761000000,"agent":"claude"}`,
		`beta.json` + "\t" + `{"state":"idle","time":1761000100}`,
		`gamma.json` + "\t" + `这不是 JSON`,                           // 坏文件：跳过
		`ghost.json` + "\t" + `{"state":"busy","time":1761000200}`, // 残留：tmux 里没有这个会话
		`epsilon.json` + "\t" + `{"state":"asleep","time":5}`,      // 状态值不认识：当没有
		`没有制表符的行`,
		"",
	}, "\n")
	fx := newFixtureExec(t, nil, execWithBusy(f, statesOut, "", 0, &calls))
	st, _, _ := run(t, fx.dir)
	newSessions(t, st, "alpha", "beta", "gamma", "delta", "epsilon")

	raw, items, rawItems := getSessionsRaw(t, st)
	got := map[string]busyItem{}
	for _, it := range items {
		got[it.Name] = it
	}
	want := map[string]busyItem{
		"alpha":   {Name: "alpha", Busy: "busy", BusyTime: 1761000000},
		"beta":    {Name: "beta", Busy: "idle", BusyTime: 1761000100},
		"gamma":   {Name: "gamma"},
		"delta":   {Name: "delta"},
		"epsilon": {Name: "epsilon"},
	}
	if len(got) != len(want) {
		t.Fatalf("会话 %+v，期望 %+v（残留的 ghost 不该出现）: %s", got, want, raw)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("会话 %s = %+v，期望 %+v", n, got[n], w)
		}
	}
	// 字段一直在（读不到时是空串和 0，不是缺字段），原有字段不丢
	for _, ri := range rawItems {
		for _, k := range []string{"name", "attached", "windows", "created", "activity", "busy", "busy_time"} {
			if _, ok := ri[k]; !ok {
				t.Errorf("会话 %v 缺字段 %s: %s", ri["name"], k, raw)
			}
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("一次 /sessions 该读一次状态，读了 %d 次", n)
	}
}

// 读状态失败（命令出错、非零退出）：/sessions 照常 200 出列表，状态全空。
func TestSessionsBusyReadFailureKeepsList(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		stderr string
		code   int
	}{
		{"非零退出", "", "sh: 1: boom", 1},
		{"非零退出还带输出", `alpha.json` + "\t" + `{"state":"busy","time":1}`, "", 2},
		{"命令不存在", "", "sh: command not found", 127},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTmux()
			var calls int32
			fx := newFixtureExec(t, nil, execWithBusy(f, c.out, c.stderr, c.code, &calls))
			st, _, _ := run(t, fx.dir)
			newSessions(t, st, "alpha", "beta")

			_, items, _ := getSessionsRaw(t, st)
			if len(items) != 2 {
				t.Fatalf("读状态失败时列表也该照常：%+v", items)
			}
			for _, it := range items {
				if it.Busy != "" || it.BusyTime != 0 {
					t.Errorf("读状态失败时状态该为空：%+v", it)
				}
			}
			if atomic.LoadInt32(&calls) == 0 {
				t.Error("应该试过读状态")
			}
		})
	}
}

// 状态目录不存在 / 空：命令输出为空，状态全空，列表照常。
func TestSessionsBusyNoStateFiles(t *testing.T) {
	f := newFakeTmux()
	var calls int32
	fx := newFixtureExec(t, nil, execWithBusy(f, "", "", 0, &calls))
	st, _, _ := run(t, fx.dir)
	newSessions(t, st, "alpha")
	_, items, _ := getSessionsRaw(t, st)
	if len(items) != 1 || items[0].Busy != "" || items[0].BusyTime != 0 {
		t.Errorf("没有状态文件：%+v", items)
	}
}

// tmux 没有会话（无服务）：回空列表，不必读状态。
func TestSessionsBusyNotReadWhenNoSessions(t *testing.T) {
	f := newFakeTmux()
	var calls int32
	fx := newFixtureExec(t, nil, execWithBusy(f, `alpha.json`+"\t"+`{"state":"busy","time":1}`, "", 0, &calls))
	st, _, _ := run(t, fx.dir)
	resp, b := call(t, st, "GET", "/sessions?host=ubuntu")
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"sessions":[]`) {
		t.Fatalf("无服务该回空列表：%d %s", resp.StatusCode, b)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("没有会话时不该读状态，读了 %d 次", n)
	}
}

func TestParseBusyStates(t *testing.T) {
	big := strings.Repeat("x", maxBusyFileBytes+1)
	out := strings.Join([]string{
		"a.json\t" + `{"state":"busy","time":10}` + "\r", // 行尾 \r
		"b.json\t" + `{"state":"idle"}`,                  // 没有 time：0
		"c.txt\t" + `{"state":"busy","time":1}`,          // 不是 .json
		".json\t" + `{"state":"busy","time":1}`,          // 没有会话名
		"d.json\t" + `{"state":"gone","time":1}`,         // 状态值不是 busy/idle
		"e.json\t" + big,                                 // 超长
		"f.json\t",                                       // 空内容
		"g.json\t[1,2]",                                  // 不是对象
		"h.json\t" + `{"state":"busy","time":"x"}`,       // time 类型不对：整条跳过
		"no-tab-line",
		"i.json\t" + `{"state":"idle","time":99}`,
	}, "\n")
	got := parseBusyStates([]byte(out))
	want := map[string]busyInfo{
		"a": {"busy", 10},
		"b": {"idle", 0},
		"i": {"idle", 99},
	}
	if len(got) != len(want) {
		t.Fatalf("解析得到 %+v，期望 %+v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v，期望 %+v", k, got[k], w)
		}
	}
	if m := parseBusyStates(nil); m == nil || len(m) != 0 {
		t.Errorf("空输入该得到空表，得到 %v", m)
	}
}
