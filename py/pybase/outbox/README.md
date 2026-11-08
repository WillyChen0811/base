# pybase.outbox

脱敏审核：一份会话两边——云端的 **Claude**（claude CLI，只读给的代码目录）和本地的 **omlx**（本地代理，只读用户给的范围）。
用户每条话选好发给哪边，回复可以改了再转给另一边；发给 Claude 的每一条先过闸门。终端发起的会话可以把用户看过的话转回给发起方。
零第三方依赖；目录、代理命令、敏感串来源、Claude 能读的目录和附加提示词全由调用方给。

```python
from pybase.outbox import ClaudeAgent, Gate, OmlxAgent, Outbox, names_source, write_config

box = Outbox("<数据目录>",
             omlx=OmlxAgent(["<本地代理可执行文件>"], timeout=3600),
             claude=ClaudeAgent(["<claude 可执行文件>"], cwd="<工作目录>", add_dirs=["<另外能读的目录>"]),
             gate=Gate([names_source("<资料目录>", generic={"报告"}, code_patterns=[r"\d{6}[A-Z]\d{4}"])]),
             claude_prompt="<给 Claude 的附加提示词>", id_prefix="conv-", config_path="<配置文件>")
box.recover()                                    # 服务启动时：跑它的进程没了还标着在跑的轮标成出错
c = box.create("omlx", "要问的话", scope=["/abs/dir", "/abs/file.txt"])   # 新建并发给 omlx（也可以从 "claude" 开始）
box.get(c["id"])                                 # 轮询：rounds[-1].status 是 running 就过一会儿再看；steps 是每一步
box.send(c["id"], "claude", "omlx 说……", model="sonnet", force=False)    # 发给 Claude：有命中抛 GateHits(.hits)
box.stop(c["id"], "omlx")
write_config("<配置文件>", data_dir="<数据目录>", omlx=..., claude=..., enabled=True)   # 给命令行入口找到同一份库
```

- 一轮：`side`（`claude` / `omlx` / `requester` = 转回给发起方）、`model`、`prompt`、`reply`、`status`（`running` / `done` /
  `failed`）、`error`、`source`（`window` / `terminal`）+ `requester`、`steps`。两边可以同时跑，每边同时最多一轮。
- 接着聊：每边各存最近一轮跑完回的会话号，下一轮 `--resume` 它。
- 出错一律 `OutboxError` 子类（`code`：`bad_input`、`not_found`、`conflict`、`gate_hits`）；闸门查不了是 `GateUnavailable`，
  这时发给 Claude、转回发起方一律不做（带 `force` 也不做）。

## 终端请 omlx 协查

```
python -m pybase.outbox.ask [--title 标题] [--scope 路径]... [--model 模型] [--from 名字] [--wait] [--timeout 秒] 提示词文件|-
python -m pybase.outbox.ask --wait-reply 会话号 [--timeout 秒]
```

建一份「来自终端」的会话、直接发给 omlx（后台进程跑，命令退出了照跑）。不带 `--wait` 打印会话号和状态就退出；
`--wait` / `--wait-reply` 一直等到用户在窗口里转回，stdout 只有转回的那段话。omlx 的回复和过程拿不到。
库在哪、代理是哪个命令从配置文件来：`--config` > `PYBASE_OUTBOX_CONFIG` > `~/.config/pybase/outbox.json`（建 Outbox 的那边写，见 config.py）。

## omlx 一边：本地代理的调用契约

```
<命令…> --investigate --stream [--model 模型] [--scope 路径]... --system-file 文件 [--resume 会话号] -
<命令…> --list-models          # 一行一个模型名
<命令…> --default-model        # 一行：不给 --model 时用的模型
```

- `--scope`：可以看的目录或文件（绝对路径，可多次）；代理只读、只看这些（怎么限住由代理定）。
- `--system-file`：附加系统提示的文件（UTF-8，`{范围}` 已填好；用完由本库删掉）。
- `--resume`：接着这个会话问（上一轮回的 `session_id`）；第一轮不带。同一个号接着用、或每轮另起一个号都行，本库只认每轮回来的那个。
- 最后的 `-`：提示词从 stdin 读（UTF-8）。
- `--stream`：stdout 逐行打 claude CLI `--output-format stream-json` 的事件（本库据此逐条存「每一步」），**最后一行**是结果 JSON：
  形状同 claude 的 result 事件——`result`（回复文字）、`session_id`、`is_error`、`subtype`（其余字段照收不认；`omlx` 对象里的
  `denied`/`elapsed`/`model` 记进这一轮的附带信息）。
- 退出码 0 = 答完；非 0 且没有那行结果 = 没跑起来，原因在 stderr（本库取最后 20 行显示给用户）。

## Claude 一边

`claude -p --output-format stream-json --restricted --tools Read,Glob,Grep --strict-mcp-config --disable-slash-commands
--settings '{"disableAllHooks": true}' --permission-mode dontAsk --disallowedTools …`，提示词走 stdin，`--add-dir` 给的目录、
`--append-system-prompt`、`--model`、`--resume`；环境去掉 `ANTHROPIC_*` / `CLAUDECODE` / `CLAUDE_CODE_*`，
`CLAUDE_CODE_TMPDIR` 指到本库的空目录。system/init 事件里多出别的工具或带了 MCP 服务就马上停下、这一轮记出错。各项的理由见 claude.py。

子进程都自成进程组，停下 / 超时整组杀。
