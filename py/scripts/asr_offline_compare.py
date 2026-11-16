"""Mac-only, opt-in real oMLX comparison using exclusively synthesized speech.

Does not read app settings, recordings or customer data. Never prints the API key.
This measures one clean TTS sample, not real meeting quality or denoising quality.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import unicodedata
import wave

from pybase.asr.remote import transcribe_remote


REFERENCE = """我们现在开始一次完全虚构的软件测试会议。下面的内容只用于语音识别对照，不包含真实客户资料。
请先打开用户管理页面。新增用户时，需要填写姓名、账号和所属部门。账号必须唯一，重复时应提示原因，不能直接覆盖原来的用户。
我问的是新增账号，而不是修改密码。修改密码稍后再讨论。保存成功以后，请回到列表，检查刚才创建的用户能否通过姓名搜索出来。
列表支持按部门筛选吗？支持。那我们选择研发部门，再看看其他部门的用户是否还显示在结果里。筛选条件清空以后，全部用户应该重新出现。
接下来讨论权限配置。一个用户可以同时拥有多个角色，各个角色分别授予不同页面的访问权限。修改角色之后，已有登录会话如何生效，还需要进一步核对。
这里提到的隐藏按钮，不等于服务端已经禁止访问。即使页面上看不到删除按钮，也应该验证直接发送请求时是否仍然会删除数据。
先停一下，窗外好像下雨了，桌上的水杯也该换一个位置。这几句话只是无关闲聊，不是软件需求，但语音识别本身仍应如实记录。
现在回到刚才的权限问题。普通用户只能查看自己的记录，管理员可以查看所在部门的记录。不同部门之间是否完全隔离，需要继续确认。
然后检查文件上传。上传失败时要显示完整原因，允许用户重试，不能让窗口无声关闭。重新打开页面以后，已经上传成功的附件应当仍然存在。
这里还有一个跨功能的场景。用户上传附件之后，如果管理员撤销他的角色，再次访问原附件时，文件访问权限与角色配置需要一起检查。
请注意，这些对话只是在描述测试过程和待核对的问题。有人口头说已经支持某项功能，并不能直接证明这项功能已经通过验证。
最后回看操作记录。账号新增、角色变更和附件删除都应该能够追溯到操作人以及发生时间。查询记录的时候，时间范围不能因为切换页面而意外变化。
今天这段合成录音到这里结束。我们将使用完全相同的一份音频，比较逐句识别与较大音频窗口识别的文字准确率和处理耗时。"""


def normalized(text: str) -> str:
    """CER ignores Unicode punctuation, symbols, spaces and letter case only."""
    return "".join(c for c in unicodedata.normalize("NFC", text).casefold()
                   if unicodedata.category(c)[0] in ("L", "N"))


def distance(reference: str, hypothesis: str) -> int:
    previous = list(range(len(hypothesis) + 1))
    for i, a in enumerate(reference, 1):
        current = [i]
        for j, b in enumerate(hypothesis, 1):
            current.append(min(current[-1] + 1, previous[j] + 1,
                               previous[j - 1] + (a != b)))
        previous = current
    return previous[-1]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.environ.get("ASR_COMPARE_BASE_URL", ""))
    parser.add_argument("--key-env", default="ASR_COMPARE_API_KEY")
    parser.add_argument("--text-model", default="Qwen3-ASR-1.7B-8bit")
    parser.add_argument("--timing-model", default="whisper-large-v3-turbo-asr-fp16")
    parser.add_argument("--voice", default="Tingting")
    parser.add_argument("--rate", type=int, default=180)
    parser.add_argument("--out", type=Path, default=Path(os.environ.get("ASR_COMPARE_OUT", "asr-compare-out")))
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("This opt-in comparison requires macOS say and afconvert.")
    if not args.base_url:
        parser.error("Provide --base-url or ASR_COMPARE_BASE_URL.")
    if args.key_env not in os.environ:
        parser.error("The selected API key environment variable is absent (an explicit empty value is allowed).")
    api_key = os.environ[args.key_env]
    args.out.mkdir(parents=True, exist_ok=True)
    reference_file = args.out / "reference.txt"
    reference_file.write_text(REFERENCE, encoding="utf-8")
    aiff, audio = args.out / "synthetic.aiff", args.out / "synthetic-16k.wav"
    report = {
        "scope": "Single clean synthetic Chinese voice only; not representative of real meetings or denoising.",
        "normalization": "NFC, casefold, retain Unicode letters/numbers; no spelling or numeral corrections.",
        "voice": args.voice, "rate": args.rate, "text_model": args.text_model,
        "timing_model": args.timing_model, "reference": REFERENCE, "modes": [],
        "caveat": "Sequential order can include model cold-load/cache effects; time is end-to-end. Window timestamps are audio ranges, not sentence times.",
    }
    try:
        subprocess.run(["/usr/bin/say", "-v", args.voice, "-r", str(args.rate),
                        "-f", str(reference_file), "-o", str(aiff)], check=True,
                       capture_output=True, text=True, timeout=300)
        subprocess.run(["/usr/bin/afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1",
                        str(aiff), str(audio)], check=True, capture_output=True, text=True, timeout=60)
        aiff.unlink()
        with wave.open(str(audio), "rb") as wav:
            report["audio_duration_s"] = wav.getnframes() / wav.getframerate()
        expected = normalized(REFERENCE)
        report["reference_characters"] = len(expected)
        for label, window in (("sentence", None), ("window-30s", 30_000), ("window-60s", 60_000)):
            print(f"Running {label} on synthetic audio", flush=True)
            started = time.monotonic()
            row = {"mode": label, "text_window_ms": window}
            try:
                result = transcribe_remote(audio, base_url=args.base_url, api_key=api_key,
                    timing_model=args.timing_model, text_model=args.text_model, language="zh",
                    concurrency=1, text_window_ms=window, checkpoint_dir=args.out / f"ckpt-{label}")
                text = "\n".join(c.text for c in result.cues)
                errors = distance(expected, normalized(text))
                row.update(ok=True, text=text, character_errors=errors, cer=errors / len(expected),
                           cues=[c.to_dict() for c in result.cues])
            except Exception as exc:
                row.update(ok=False, error=safe_error(exc, api_key, args.base_url))
            row["elapsed_s"] = round(time.monotonic() - started, 3)
            report["modes"].append(row)
            (args.out / "comparison.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
            print(f"{label}: CER={row.get('cer', 'failed')}, elapsed={row['elapsed_s']}s", flush=True)
    except Exception as exc:
        report["setup_error"] = safe_error(exc, api_key, args.base_url)
    (args.out / "comparison.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    lines = [report["scope"], report["normalization"], report["caveat"], "", "Reference:", REFERENCE]
    if report.get("setup_error"):
        lines += ["", "Setup failed: " + report["setup_error"]]
    for row in report["modes"]:
        lines += ["", f"{row['mode']} | CER {row.get('cer', 'failed')} | {row['elapsed_s']} seconds",
                  row.get("text", row.get("error", ""))]
    (args.out / "comparison.txt").write_text("\n".join(lines), encoding="utf-8")
    return 0 if len(report["modes"]) == 3 and all(r["ok"] for r in report["modes"]) else 1


def safe_error(exc: Exception, api_key: str, base_url: str) -> str:
    message = f"{type(exc).__name__}: {exc}"
    if api_key:
        message = message.replace(api_key, "[redacted]")
    return message.replace(base_url, "[endpoint]")


if __name__ == "__main__":
    raise SystemExit(main())
