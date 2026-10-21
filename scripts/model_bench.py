#!/usr/bin/env python3
"""Tabbit 网关模型量化测试：延迟、TTFT、吐字速率、上下文阶梯、工具调用、并发、额度消耗。

只依赖标准库。每个用例都重启一次网关进程：上游房间（chat_session_id）是**按账号**
缓存 30 分钟复用的，不重启的话上一个用例的对话会留在房间里污染下一个用例
（实测：问「我上一条问的是什么」能答出上一个用例的内容）。重启 + 每次只放一个账号
进 auth_dir，顺带把额度摊到各账号上。

    python3 scripts/model_bench.py --gateway-cmd "/path/tab2api -config ./config.json" --cwd /tmp/bench

不传 --gateway-cmd 就当外部已经起好网关（不隔离，只适合单用例调试）。
"""

import argparse
import glob
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

CN_BASE = "https://web.tabbit.com"
QUOTA_PATH = "/api/commerce/quota/v1/usage"


def post_chat(base, key, body, timeout=240):
    """发一次请求。流式返回首**正文**帧耗时作 TTFT（思考链不算首字）。"""
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key},
    )
    start = time.perf_counter()
    ttft = None
    content, reasoning, tools = [], [], []
    usage = None
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            if "event-stream" in resp.headers.get("Content-Type", ""):
                for raw in resp:
                    line = raw.decode("utf-8", "replace").strip()
                    if not line.startswith("data:"):
                        continue
                    data = line[5:].strip()
                    if data == "[DONE]":
                        break
                    try:
                        frame = json.loads(data)
                    except ValueError:
                        continue
                    if frame.get("usage"):
                        usage = frame["usage"]
                    for choice in frame.get("choices") or []:
                        delta = choice.get("delta") or {}
                        if delta.get("reasoning_content"):
                            reasoning.append(delta["reasoning_content"])
                        for call in delta.get("tool_calls") or []:
                            name = (call.get("function") or {}).get("name")
                            if name:
                                tools.append(name)
                        if delta.get("content"):
                            if ttft is None:
                                ttft = time.perf_counter() - start
                            content.append(delta["content"])
                elapsed = time.perf_counter() - start
            else:
                obj = json.loads(resp.read() or b"{}")
                elapsed = time.perf_counter() - start
                usage = obj.get("usage")
                for choice in obj.get("choices") or []:
                    msg = choice.get("message") or {}
                    content.append(msg.get("content") or "")
                    reasoning.append(msg.get("reasoning_content") or "")
                    for call in msg.get("tool_calls") or []:
                        tools.append((call.get("function") or {}).get("name") or "")
                ttft = None
            return status, "".join(content), "".join(reasoning), tools, ttft, elapsed, usage, None
    except urllib.error.HTTPError as e:
        return e.code, "", "", [], None, time.perf_counter() - start, None, e.read().decode("utf-8", "replace")[:200]
    except Exception as e:  # noqa: BLE001 — 超时/连接断都算失败样本
        return 0, "", "", [], None, time.perf_counter() - start, None, repr(e)[:200]


def quota_used(uid):
    """上游额度：已用百分比。查不到返回 None，不猜。"""
    url = f"{CN_BASE}{QUOTA_PATH}?user_id={uid}&timezone=Asia/Shanghai"
    try:
        with urllib.request.urlopen(url, timeout=15) as resp:
            return float(str(json.loads(resp.read())["usage_percentage"]).rstrip("%"))
    except Exception:  # noqa: BLE001
        return None


def filler(n, needle):
    """n 个汉字的填充文，needle 在正中间（避免首尾位置带来的检索偏置）。"""
    base = "电梯定期检验记录显示设备运行平稳各部件磨损在允许范围内维保单位已完成季度保养。"
    pad = (base * (n // len(base) + 1))[:n]
    mid = len(pad) // 2
    return pad[:mid] + needle + pad[mid:]


def cases():
    out = [
        ("zh-short", False, {"messages": [{"role": "user", "content": "用三句话解释电梯限速器与安全钳的配合关系。"}]}, "限速"),
        ("zh-stream", True, {"messages": [{"role": "user", "content": "用三句话解释电梯限速器与安全钳的配合关系。"}]}, "安全钳"),
        ("en-short", True, {"messages": [{"role": "user", "content": "In two sentences, what does an elevator governor do?"}]}, "speed"),
        ("math", True, {"messages": [{"role": "user", "content": "轿厢自重800kg，额定载重1000kg，平衡系数0.45。配重质量是多少？只要数值和单位。"}]}, "1250"),
        ("json", False, {"messages": [{"role": "user", "content": '只输出严格 JSON：{"model":"<型号>","risk":<1-5整数>,"reason":"<10字内>"}。'}]}, "{"),
        ("code", True, {"messages": [{"role": "user", "content": "写一个 Python 函数 is_prime(n)，只输出代码块。"}]}, "def is_prime"),
        ("reasoning", True, {"messages": [{"role": "user", "content": "甲乙丙中只有一人说真话。甲：乙在说谎。乙：丙在说谎。丙：甲和乙都在说谎。谁说真话？只答人名。"}]}, "乙"),
        ("system-history", True, {"messages": [
            {"role": "system", "content": "你是巨菱电梯的技术助理。"},
            {"role": "user", "content": "上行超速保护装置属于哪个部件？"},
            {"role": "assistant", "content": "属于安全保护装置。"},
            {"role": "user", "content": "它和限速器是什么关系？一句话。"},
        ]}, "限速器"),
        ("tools", False, {"messages": [{"role": "user", "content": "查一下 DL-7749 这台电梯的最近检验日期。"}],
                          "tools": [{"type": "function", "function": {
                              "name": "lookup_elevator", "description": "按编号查电梯档案",
                              "parameters": {"type": "object", "properties": {"code": {"type": "string"}}, "required": ["code"]}}}]},
         "lookup_elevator"),
    ]
    # 上下文阶梯（网关的裁剪预算是 16000 字符，上游 2 万字符必失败）
    for size in (1000, 4000, 8000, 12000, 16000):
        out.append((f"ctx-{size}", True, {"messages": [{"role": "user", "content":
                    filler(size, "【电梯编号 DL-7749】") + "\n\n文中提到的电梯编号是什么？只回答编号本身。"}]}, "DL-7749"))
    return out


class Gateway:
    """按用例重启网关，让每个用例拿到全新的上游房间。"""

    def __init__(self, cmd, cwd, base, auth_src, auth_dir, acct_cycle):
        self.cmd, self.cwd, self.base = cmd, cwd, base
        self.auth_src, self.auth_dir, self.accts = auth_src, auth_dir, acct_cycle
        self.proc = None
        self.index = 0

    def _kill(self):
        if self.proc and self.proc.poll() is None:
            os.killpg(os.getpgid(self.proc.pid), signal.SIGTERM)
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(os.getpgid(self.proc.pid), signal.SIGKILL)
        self.proc = None

    def restart(self):
        """换一个账号、起新进程；等 /healthz 通了返回该账号 uid。"""
        self._kill()
        uid = self.accts[self.index % len(self.accts)]
        self.index += 1
        shutil.rmtree(self.auth_dir, ignore_errors=True)
        os.makedirs(self.auth_dir, exist_ok=True)
        shutil.copy(f"{self.auth_src}/{uid}.json", self.auth_dir)
        self.proc = subprocess.Popen(self.cmd, shell=True, cwd=self.cwd,
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                     start_new_session=True)
        for _ in range(80):
            try:
                with urllib.request.urlopen(self.base + "/healthz", timeout=2) as resp:
                    if resp.status == 200:
                        return uid
            except Exception:  # noqa: BLE001
                time.sleep(0.25)
        raise RuntimeError("网关没起来")

    def stop(self):
        self._kill()


def run_case(base, key, name, stream, body, expect, model, gw):
    uid = gw.restart() if gw else ""
    status, text, reasoning, tools, ttft, elapsed, usage, err = post_chat(
        base, key, dict(body, model=model, stream=stream) if stream else dict(body, model=model))
    hit_text = expect is not None and expect.lower() in (text + "".join(tools)).lower()
    return {
        "case": name, "status": status, "pass": hit_text if expect is not None else None,
        "expect": expect, "ttft_s": round(ttft, 3) if ttft else None,
        "elapsed_s": round(elapsed, 3),
        "prompt_chars": sum(len(m.get("content") or "") for m in body["messages"]),
        "answer_chars": len(text), "reasoning_chars": len(reasoning),
        "cjk_chars": sum(1 for ch in text + reasoning if ord(ch) > 0x2E80),
        "ascii_chars": sum(1 for ch in text + reasoning if ord(ch) <= 0x2E80),
        "tool_calls": tools or None,
        "usage": usage, "account": uid,
        "answer_head": text[:60].replace("\n", " "),
        "error": err,
    }


def quant(values, p):
    values = sorted(v for v in values if v is not None)
    if not values:
        return None
    return round(values[min(len(values) - 1, int(round((p / 100) * (len(values) - 1))))], 2)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:7890")
    ap.add_argument("--key", default=os.environ.get("TABBIT_API_KEY", ""))
    ap.add_argument("--model", default="cn:default")
    ap.add_argument("--from", dest="skip_head", type=int, default=0, help="跳过前 N 个用例（续跑）")
    ap.add_argument("--skip-long", action="store_true")
    ap.add_argument("--only", default="", help="只跑名字前缀匹配的用例，逗号分隔")
    ap.add_argument("--concurrency", type=int, default=3)
    ap.add_argument("--gateway-cmd", default="")
    ap.add_argument("--cwd", default="/root/.hermes/cache/scratch/tabbit-bench")
    ap.add_argument("--auth-src", default="/opt/tabbit2api-panel/auths")
    ap.add_argument("--auth-dir", default="/root/.hermes/cache/scratch/tabbit-bench/auths")
    ap.add_argument("--out", default="/root/.hermes/cache/scratch/tabbit-bench/results.jsonl")
    args = ap.parse_args()

    accts = sorted(os.path.basename(p)[:-5] for p in glob.glob(args.auth_src + "/*.json"))
    gw = Gateway(args.gateway_cmd, args.cwd, args.base, args.auth_src, args.auth_dir, accts) if args.gateway_cmd else None
    quota_start = {u: quota_used(u) for u in accts} if gw else {}
    rows = []
    try:
        for i, (name, stream, body, expect) in enumerate(cases()):
            if i < args.skip_head or (args.skip_long and name.startswith("ctx-")):
                continue
            if args.only and not any(name.startswith(p) for p in args.only.split(",")):
                continue
            row = run_case(args.base, args.key, name, stream, body, expect, args.model, gw)
            rows.append(row)
            print(json.dumps(row, ensure_ascii=False), flush=True)

        # 并发：同一账号房间共享，这里只测网关是否串号/限流/丢请求
        if args.concurrency > 1:
            if gw:
                gw.restart()
            start = time.perf_counter()
            with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
                futures = [pool.submit(run_case, args.base, args.key, f"concurrent-{n}", True,
                                       {"messages": [{"role": "user", "content": f"只输出数字 {n}，不要别的字。"}]},
                                       str(n), args.model, None) for n in range(args.concurrency)]
                conc = [f.result() for f in futures]
            wall = round(time.perf_counter() - start, 3)
            for row in conc:
                row["conc_wall_s"] = wall
                rows.append(row)
                print(json.dumps(row, ensure_ascii=False), flush=True)
    finally:
        if gw:
            gw.stop()

    quota_end = {u: quota_used(u) for u in accts} if gw else {}
    with open(args.out, "w") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False) + "\n")

    ok = [r for r in rows if r["status"] == 200]
    graded = [r for r in rows if r["pass"] is not None]
    print(f"\n=== {len(rows)} 请求，成功 {len(ok)}，判定通过 {sum(1 for r in graded if r['pass'])}/{len(graded)} ===")
    print("TTFT p50/p95: %s / %s s" % (quant([r["ttft_s"] for r in ok], 50), quant([r["ttft_s"] for r in ok], 95)))
    print("总延迟 p50/p95: %s / %s s" % (quant([r["elapsed_s"] for r in rows], 50), quant([r["elapsed_s"] for r in rows], 95)))
    if quota_start:
        print("额度（已用百分比，延迟刷新，仅供量级参考）: 起 %s 止 %s" % (
            {k[:8]: v for k, v in quota_start.items()}, {k[:8]: v for k, v in quota_end.items()}))
    for r in rows:
        if r["status"] != 200:
            print("  失败 %s: HTTP %s %s" % (r["case"], r["status"], r["error"]))
    print("结果: " + args.out)


if __name__ == "__main__":
    sys.exit(main())
