# pybase.outbox

脱敏审核：本地代理在用户自己的机器上只读调查、可以追问；用户把回复整理成「放行稿」、过闸门、放行后才落到外发目录。
零第三方依赖；目录、代理命令、敏感串来源全由调用方给。

```python
from pybase.outbox import CommandAgent, Gate, Outbox, names_source

box = Outbox("<数据目录>", approved_dir="<已放行目录>", rejected_dir="<已退回目录>",
             agent=CommandAgent(["<代理可执行文件>"], timeout=3600),
             gate=Gate([names_source("<资料目录>", generic={"报告"}, code_patterns=[r"\d{6}[A-Z]\d{4}"])]),
             id_prefix="inv-")
box.recover()                                   # 服务启动时：进程没了还标着在跑的轮标成出错
inv = box.create("标题", "用途", ["/abs/dir", "/abs/file.txt"], "要问的话", origin="user", send=True)
box.get(inv["id"])                              # 轮询：status 是 running 就过一会儿再看
box.send(inv["id"], "接着问一句")                # 追问（接着上一轮的会话）
box.save_draft(inv["id"], "改过的放行稿")
box.check(inv["id"])                            # 闸门命中串（只给用户看）
box.approve(inv["id"], force=False)             # 有命中抛 GateHits(.hits)；写 <已放行目录>/<id>.md（0600）
```

状态：调查 `draft` → `running` → `waiting` →（追问）`running` …；`draft`/`waiting` → `approved` 或 `rejected`（只读）。
一轮：`pending`（待发）/ `running` / `done` / `failed`。出错一律 `OutboxError` 子类（`code`：`bad_input`、`not_found`、
`conflict`、`gate_hits`）；闸门查不了是 `GateUnavailable`，这时不放行、不退回。

## 本地代理的调用契约

```
<命令…> --investigate [--scope 路径]... --system-file 文件 [--resume 会话号] -
```

- `--scope`：可以看的目录或文件（绝对路径，可多次）；代理只读、只看这些（怎么限住由代理定）。
- `--system-file`：附加系统提示的文件（UTF-8，`{范围}` 已填好；用完由本库删掉）。
- `--resume`：接着这个会话问（上一轮回的 `session_id`）；第一轮不带。代理每轮回自己的会话号即可
  （同一个号接着用、或每轮另起一个号都行，本库只认每轮回来的那个）。
- 最后的 `-`：提示词从 stdin 读（UTF-8）。
- stdout 最后一行是 JSON 对象，形状同 claude CLI `--output-format json` 的结果：`result`（回复文字）、`session_id`、
  `is_error`、`subtype`（其余字段照收不认；`omlx` 对象里的 `denied`/`elapsed`/`model` 记进这一轮的附带信息）。
- 退出码 0 = 答完；非 0 且没有那行 JSON = 没跑起来，原因在 stderr（本库取最后 20 行显示给用户）。

子进程自成进程组，停下 / 超时整组杀。
