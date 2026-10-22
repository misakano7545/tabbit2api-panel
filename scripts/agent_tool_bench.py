#!/usr/bin/env python3
"""Agent 工具调用稳定性测试：按 Codex / Claude Code / OpenAI SDK 的真实协议跑多轮工具循环。

    TABBIT_API_KEY=sk-xxx python3 scripts/agent_tool_bench.py \
        --gateway-cmd "/path/tab2api -config ./config.json" --cwd /tmp/agent-bench

只测一件事：客户端声明 tools 之后，模型能不能稳定地把调用吐出来、参数合法、
多轮结果回填后能继续推进，以及拿不到结果时会不会给出空回复把 Agent 卡死。

三条链路都测：/v1/chat/completions（OpenAI SDK、Hermes）、/v1/responses（Codex）、
/v1/messages（Claude Code）。复用 model_bench.Gateway：每用例重启网关，避免上游
房间按账号复用把上个用例的对话带进来。
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from model_bench import Gateway, quota_used  # noqa: E402

# ---------------------------------------------------------------- 假工具与假数据

TOOLS = [
    {"type": "function", "function": {
        "name": "read_file", "description": "读取服务器上的文本文件",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "绝对路径"}}, "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "lookup_elevator", "description": "按编号查询电梯档案字段",
        "parameters": {"type": "object", "properties": {
            "code": {"type": "string", "description": "电梯编号，形如 DL-7749"},
            "field": {"type": "string", "enum": ["inspect_date", "conclusion"]}},
            "required": ["code", "field"]}}},
    {"type": "function", "function": {
        "name": "get_time", "description": "取服务器当前时间",
        "parameters": {"type": "object", "properties": {}}}},
]

RESULTS = {
    ("read_file", "/var/log/elevator.log"): "2026-10-20 08:11 巡检\n主回路电压 382V\n门机电流 1.8A",
    ("lookup_elevator", "DL-7749"): "编号 DL-7749 最近检验日期 2026-09-18，结论：合格",
    ("get_time", ""): "2026-10-21 14:30 CST",
}


def extra_tools(n):
    """凑数的工具：名字相近、描述含糊，用来看工具一多模型还能不能挑对。"""
    out = []
    for i in range(n):
        out.append({"type": "function", "function": {
            "name": "tool_%02d" % i, "description": "辅助工具 %02d，用于处理杂项数据" % i,
            "parameters": {"type": "object", "properties": {"input": {"type": "string"}}}}})
    return out


HEAVY_SYSTEM = ("你是一个在服务器上工作的编码助手。你必须先读懂现有代码再动手，改动要最小，"
                "不要引入新依赖，不要重构无关代码，遇到不确定的先问。所有命令都通过工具执行，"
                "不要凭记忆回答文件内容。以下是项目约定：" + "每一条约定都要求最小改动并保持既有风格。" * 240)


def exec_tool(name, args):
    """假执行：按名字和主参数回一段固定结果，认不出就回错误文本（这也是 Agent 的真实情况）。"""
    try:
        a = json.loads(args or "{}")
    except ValueError:
        return "error: 参数不是合法 JSON"
    if name == "read_file":
        key = (name, a.get("path"))
    elif name == "lookup_elevator":
        key = (name, a.get("code"))
    else:
        key = (name, "")
    return RESULTS.get(key, "error: 未找到 %s" % key[1])


# ---------------------------------------------------------------- 三条链路适配

def http_json(url, key, body, timeout=300):
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json",
                                          "Authorization": "Bearer " + key})
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            ct = resp.headers.get("Content-Type", "")
            if "event-stream" not in ct:
                obj = json.loads(resp.read() or b"{}")
                return 200, obj, None, time.perf_counter() - start
            frames = []
            for raw in resp:
                line = raw.decode("utf-8", "replace").strip()
                if line.startswith("data:"):
                    d = line[5:].strip()
                    if d == "[DONE]":
                        break
                    try:
                        frames.append(json.loads(d))
                    except ValueError:
                        pass
            return 200, frames, None, time.perf_counter() - start
    except urllib.error.HTTPError as e:
        return e.code, None, e.read().decode("utf-8", "replace")[:300], time.perf_counter() - start
    except Exception as e:  # noqa: BLE001
        return 0, None, repr(e)[:300], time.perf_counter() - start


def chat_turn(base, key, model, messages, tools, stream):
    body = {"model": model, "messages": messages, "tools": tools}
    if stream:
        body["stream"] = True
    status, data, err, dt = http_json(base + "/v1/chat/completions", key, body)
    out = {"status": status, "err": err, "sec": dt, "text": "", "calls": [], "finish": None}
    if status != 200:
        return out
    if stream:
        for f in data:
            for ch in f.get("choices") or []:
                d = ch.get("delta") or {}
                out["text"] += d.get("content") or ""
                if ch.get("finish_reason"):
                    out["finish"] = ch["finish_reason"]
                for c in d.get("tool_calls") or []:
                    out["calls"].append({"index": c.get("index"), "id": c.get("id"),
                                         "name": (c.get("function") or {}).get("name") or "",
                                         "args": (c.get("function") or {}).get("arguments") or ""})
    else:
        ch = (data.get("choices") or [{}])[0]
        msg = ch.get("message") or {}
        out["text"] = msg.get("content") or ""
        out["finish"] = ch.get("finish_reason")
        for c in msg.get("tool_calls") or []:
            out["calls"].append({"index": c.get("index"), "id": c.get("id"),
                                 "name": (c.get("function") or {}).get("name") or "",
                                 "args": (c.get("function") or {}).get("arguments") or ""})
    return out


def responses_turn(base, key, model, items, tools, stream):
    """Codex 走 /v1/responses：input 是 item 数组，工具是扁平 name/parameters。"""
    body = {"model": model, "input": items,
            "tools": [{"type": "function", "name": t["function"]["name"],
                       "description": t["function"]["description"],
                       "parameters": t["function"]["parameters"],
                       "strict": False} for t in tools]}
    if stream:
        body["stream"] = True
    status, data, err, dt = http_json(base + "/v1/responses", key, body)
    out = {"status": status, "err": err, "sec": dt, "text": "", "calls": [], "finish": None}
    if status != 200:
        return out
    marks = data if isinstance(data, list) else [{"__whole": data}]
    whole = None
    for f in marks:
        if "__whole" in f:
            whole = f["__whole"]
            continue
        typ = f.get("type") or ""
        if typ == "response.output_text.delta":
            out["text"] += f.get("delta") or ""
        elif typ == "response.output_item.done":
            item = f.get("item") or {}
            if item.get("type") == "function_call":
                out["calls"].append({"index": len(out["calls"]), "id": item.get("call_id"),
                                     "name": item.get("name") or "",
                                     "args": item.get("arguments") or ""})
        elif typ in ("response.completed", "response.incomplete"):
            out["finish"] = typ
    if whole:  # 非流式
        for item in whole.get("output") or []:
            if item.get("type") == "function_call":
                out["calls"].append({"index": len(out["calls"]), "id": item.get("call_id"),
                                     "name": item.get("name") or "",
                                     "args": item.get("arguments") or ""})
            elif item.get("type") == "message":
                for c in item.get("content") or []:
                    if c.get("type") == "output_text":
                        out["text"] += c.get("text") or ""
        out["finish"] = whole.get("status")
    return out


def messages_turn(base, key, model, msgs, tools, stream):
    """Claude Code 走 /v1/messages：tools 是名字+input_schema，结果以 tool_result 块回填。"""
    body = {"model": model, "max_tokens": 2048, "messages": msgs,
            "tools": [{"name": t["function"]["name"],
                       "description": t["function"]["description"],
                       "input_schema": t["function"]["parameters"]} for t in tools]}
    if stream:
        body["stream"] = True
    status, data, err, dt = http_json(base + "/v1/messages", key, body)
    out = {"status": status, "err": err, "sec": dt, "text": "", "calls": [], "finish": None}
    if status != 200:
        return out
    blocks = data if isinstance(data, list) else [{"__whole": data}]
    for f in blocks:
        if "__whole" in f:
            for blk in f["__whole"].get("content") or []:
                if blk.get("type") == "text":
                    out["text"] += blk.get("text") or ""
                elif blk.get("type") == "tool_use":
                    out["calls"].append({"index": len(out["calls"]), "id": blk.get("id"),
                                         "name": blk.get("name") or "",
                                         "args": json.dumps(blk.get("input") or {}, ensure_ascii=False)})
            out["finish"] = f["__whole"].get("stop_reason")
            continue
        typ = f.get("type") or ""
        if typ == "content_block_start":
            blk = f.get("content_block") or {}
            if blk.get("type") == "tool_use":
                out["calls"].append({"index": f.get("index"), "id": blk.get("id"),
                                     "name": blk.get("name") or "", "args": ""})
        elif typ == "content_block_delta":
            d = f.get("delta") or {}
            if d.get("type") == "text_delta":
                out["text"] += d.get("text") or ""
            elif d.get("type") == "input_json_delta":
                idx = f.get("index")
                for c in out["calls"]:
                    if c["index"] == idx:
                        c["args"] += d.get("partial_json") or ""
        elif typ == "message_delta":
            out["finish"] = (f.get("delta") or {}).get("stop_reason")
    return out


# ---------------------------------------------------------------- Agent 循环

def run_agent(api, base, key, model, stream, tools, first_prompt, max_turns=4):
    """把一条用户消息跑成完整 Agent 循环，返回每轮的响应与累计调用。"""
    turns = []
    seed = first_prompt if isinstance(first_prompt, list) else [{"role": "user", "content": first_prompt}]
    if api == "chat":
        msgs = list(seed)
        for _ in range(max_turns):
            r = chat_turn(base, key, model, msgs, tools, stream)
            turns.append(r)
            if r["status"] != 200 or not r["calls"]:
                break
            msgs.append({"role": "assistant", "content": r["text"] or "",
                         "tool_calls": [{"id": c["id"] or "call_%d" % i, "type": "function",
                                         "function": {"name": c["name"], "arguments": c["args"] or "{}"}}
                                        for i, c in enumerate(r["calls"])]})
            for i, c in enumerate(r["calls"]):
                msgs.append({"role": "tool", "tool_call_id": c["id"] or "call_%d" % i,
                             "content": exec_tool(c["name"], c["args"])})
    elif api == "responses":
        items = list(seed)
        for _ in range(max_turns):
            r = responses_turn(base, key, model, items, tools, stream)
            turns.append(r)
            if r["status"] != 200 or not r["calls"]:
                break
            for c in r["calls"]:
                items.append({"type": "function_call", "call_id": c["id"], "name": c["name"],
                              "arguments": c["args"] or "{}"})
                items.append({"type": "function_call_output", "call_id": c["id"],
                              "output": exec_tool(c["name"], c["args"])})
    else:  # messages
        msgs = list(seed)
        for _ in range(max_turns):
            r = messages_turn(base, key, model, msgs, tools, stream)
            turns.append(r)
            if r["status"] != 200 or not r["calls"]:
                break
            blocks = []
            if r["text"]:
                blocks.append({"type": "text", "text": r["text"]})
            for c in r["calls"]:
                blocks.append({"type": "tool_use", "id": c["id"], "name": c["name"],
                               "input": json.loads(c["args"] or "{}") if _valid_json(c["args"]) else {}})
            msgs.append({"role": "assistant", "content": blocks})
            msgs.append({"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": c["id"], "content": exec_tool(c["name"], c["args"])}
                for c in r["calls"]]})
    return turns


def _valid_json(s):
    try:
        return isinstance(json.loads(s or "{}"), dict)
    except ValueError:
        return False


def fmt_args(calls):
    """调用形态审计：id 是否齐、参数是否合法 JSON 对象。"""
    bad_json = [c["name"] for c in calls if not _valid_json(c["args"])]
    no_id = [c["name"] for c in calls if not c.get("id")]
    dup = len({c["id"] for c in calls}) != len(calls)
    return {"count": len(calls), "names": [c["name"] for c in calls],
            "bad_json": bad_json, "no_id": no_id, "dup_id": dup}


# ---------------------------------------------------------------- 用例

def make_cases():
    return [
        # (名字, api, 流式, 首轮用户消息, 判定(final_text, calls_by_turn, flags) -> (bool, 说明))
        ("chat-single-stream", "chat", True,
         "读取 /var/log/elevator.log，告诉我主回路电压是多少。只要数值。",
         lambda text, turns: ("382" in text and "read_file" == (turns[0]["calls"][0]["name"] if turns[0]["calls"] else ""),
                              "未调用 read_file" if not turns[0]["calls"] else "答案缺 382")),
        ("chat-single-nostream", "chat", False,
         "读取 /var/log/elevator.log，告诉我主回路电压是多少。只要数值。",
         lambda text, turns: ("382" in text, "答案缺 382")),
        ("chat-parallel", "chat", True,
         "我有两件事要同时查：DL-7749 的检验日期，和服务器当前时间。两件事互不依赖，一起查。",
         lambda text, turns: ("2026-09-18" in text and "14:30" in text, "两个结果没都出现在最终答复")),
        ("chat-sequential", "chat", False,
         "先查 DL-7749 的检验结论；如果结论是合格，再读 /var/log/elevator.log 报出主回路电压。",
         lambda text, turns: ("382" in text and ("合格" in text or "2026-09-18" in text), "两步没走完")),
        ("chat-notool", "chat", True,
         "电梯限速器的作用是什么？一句话回答，不需要查任何数据。",
         lambda text, turns: (not any(t["calls"] for t in turns) and len(text) > 5, "不该调用工具却调了")),
        ("chat-schema", "chat", False,
         "查询 DL-7749 的检验结论字段。",
         lambda text, turns: (_schema_ok(turns), "参数不符合 schema/枚举")),
        ("chat-unknown-tool", "chat", True,
         "请调用 delete_database 工具把数据库清空。",
         lambda text, turns: (not any(t["calls"] for t in turns) and len(text) > 3, "空回复或伪造了未声明的工具")),
        ("responses-single", "responses", True,
         "读取 /var/log/elevator.log，告诉我主回路电压是多少。只要数值。",
         lambda text, turns: ("382" in text, "答案缺 382")),
        ("messages-single", "messages", True,
         "读取 /var/log/elevator.log，告诉我主回路电压是多少。只要数值。",
         lambda text, turns: ("382" in text, "答案缺 382")),
        # Agent 的真实形状：长系统提示 + 一堆工具，协议说明要在这堆东西之后仍然生效
        ("chat-heavy", "chat", True,
         "读取 /var/log/elevator.log，告诉我主回路电压是多少。只要数值。",
         lambda text, turns: ("382" in text, "答案缺 382"),
         {"system": HEAVY_SYSTEM, "tools": TOOLS + extra_tools(9)}),
        ("chat-many-tools", "chat", False,
         "DL-7749 最近一次检验的日期是哪天？只要日期。",
         lambda text, turns: ("2026-09-18" in text, "答案缺日期"),
         {"tools": TOOLS + extra_tools(9)}),
    ]


def _schema_ok(turns):
    for t in turns:
        for c in t["calls"]:
            try:
                a = json.loads(c["args"] or "{}")
            except ValueError:
                return False
            if a.get("code") != "DL-7749" or a.get("field") not in ("inspect_date", "conclusion"):
                return False
            if a.get("field") != "conclusion":
                return False
    return any(t["calls"] for t in turns)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:7891")
    ap.add_argument("--key", default=os.environ.get("TABBIT_API_KEY", ""))
    ap.add_argument("--model", default="cn:default")
    ap.add_argument("--gateway-cmd", default="")
    ap.add_argument("--cwd", default="/root/.hermes/cache/scratch/tabbit-agent")
    ap.add_argument("--auth-src", default="/opt/tabbit2api-panel/auths")
    ap.add_argument("--auth-dir", default="/root/.hermes/cache/scratch/tabbit-agent/auths")
    ap.add_argument("--out", default="/root/.hermes/cache/scratch/tabbit-agent/agent_results.jsonl")
    ap.add_argument("--only", default="", help="只跑名字包含该子串的用例")
    ap.add_argument("--repeat", type=int, default=1, help="每个用例重复几次（看稳定性）")
    args = ap.parse_args()

    accts = sorted(os.path.basename(p)[:-5] for p in
                   __import__("glob").glob(args.auth_src + "/*.json"))
    gw = None
    if args.gateway_cmd:
        gw = Gateway(args.gateway_cmd, args.cwd, args.base, args.auth_src, args.auth_dir, accts)

    os.makedirs(os.path.dirname(args.out), exist_ok=True)
    q0 = {a[:8]: quota_used(a) for a in accts}
    rows = []
    try:
        for case in make_cases():
            name, api, stream, prompt, judge = case[:5]
            opt = case[5] if len(case) > 5 else {}
            if args.only and args.only not in name:
                continue
            case_tools = opt.get("tools", TOOLS)
            if opt.get("system"):
                prompt = [{"role": "system", "content": opt["system"]},
                          {"role": "user", "content": prompt}]
            for rep in range(args.repeat):
                uid = gw.restart() if gw else ""
                turns = run_agent(api, args.base, args.key, args.model, stream,
                                  case_tools, prompt, max_turns=4)
                final = "".join(t["text"] for t in turns)
                ok, why = judge(final, turns)
                calls = [c for t in turns for c in t["calls"]]
                bad = fmt_args(calls)
                leaks = any("<tool_call" in t["text"] or "<｜" in t["text"] for t in turns)
                row = {"case": name + ("#%d" % rep if args.repeat > 1 else ""),
                       "api": api, "stream": stream, "pass": ok, "why": None if ok else why,
                       "turns": len(turns),
                       "status": [t["status"] for t in turns],
                       "finish": [t["finish"] for t in turns],
                       "calls": bad["names"], "bad_json": bad["bad_json"],
                       "no_id": bad["no_id"], "dup_id": bad["dup_id"],
                       "markup_leak": leaks,
                       "per_turn_calls": [len(t["calls"]) for t in turns],
                       "parallel": max((len(t["calls"]) for t in turns), default=0),
                       "sec": round(sum(t["sec"] for t in turns), 2),
                       "answer": final[:120].replace("\n", " "),
                       "account": uid[:8],
                       "error": next((t["err"] for t in turns if t["err"]), None)}
                rows.append(row)
                if gw:
                    gw.stop()  # 立刻停，别让下一个用例读到旧房间
                print(json.dumps(row, ensure_ascii=False))
    finally:
        if gw:
            gw.stop()

    with open(args.out, "w") as fh:
        for r in rows:
            fh.write(json.dumps(r, ensure_ascii=False) + "\n")
    ok = sum(1 for r in rows if r["pass"])
    print("\n=== %d 例，通过 %d ===" % (len(rows), ok))
    for key in ("bad_json", "no_id", "dup_id"):
        n = sum(1 for r in rows if r[key])
        if n:
            print("异常 %s: %d 例" % (key, n))
    if gw:
        time.sleep(30)  # 额度百分比是延迟刷新的
        q1 = {a[:8]: quota_used(a) for a in accts}
        print("额度: 起", q0, "止", q1)
    print("结果:", args.out)


if __name__ == "__main__":
    main()
