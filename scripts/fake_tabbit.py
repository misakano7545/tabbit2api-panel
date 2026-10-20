#!/usr/bin/env python3
"""fake_tabbit.py 本地假 Tabbit 上游：只实现网关用到的那几个端点，供冒烟测试用。

用法：python3 scripts/fake_tabbit.py 18123
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18123


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("[fake-tabbit] " + fmt % args + "\n")

    def _json(self, payload):
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _text(self, text):
        body = text.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/chat/sign-key":
            return self._text("fake-sign-key")
        if path == "/proxy/v1/model_config/models":
            return self._json({"models": [
                {"display_name": "GPT-5.5", "model_access_type": "free_unlimited",
                 "supports_images": True, "supports_tools": True, "support_thinking": True,
                 "context_window": 200000, "max_tokens": 32000, "display_multiplier": 1},
                {"display_name": "Claude-Opus-4.7", "model_access_type": "pro_only",
                 "supports_images": True, "supports_tools": True, "support_thinking": True,
                 "context_window": 200000, "max_tokens": 32000, "display_multiplier": 2.5},
            ]})
        if path == "/api/v0/user/base-info":
            return self._json({"user_info": {"user_id": "smoke-1", "nickname": "冒烟号"}})
        if path == "/api/commerce/quota/v1/usage":
            return self._json({"usage_percentage": 12.5, "member_level": "free"})
        self.send_error(404)

    def do_POST(self):
        path = self.path.split("?")[0]
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        if path == "/panel/session":
            return self._json({"chat_session_id": "sess-smoke"})
        if path == "/api/v1/chat/completion":
            return self._chat(raw)
        self.send_error(404)

    def _chat(self, raw):
        try:
            prompt = json.loads(raw or b"{}").get("content", "")
        except Exception:
            prompt = ""
        # 声明了工具就问一次工具，否则回一段普通文本。
        if "<tool_call>" in prompt:
            pieces = ["我看一下。", '<tool_ca', 'll>{"name":"exec_command","arguments":{"cmd":"ls"}}</tool_', "call>"]
        else:
            pieces = ["你好，", "这是假上游", "的回复。"]

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        def frame(event, data):
            chunk = f"event: {event}\ndata: {json.dumps(data, ensure_ascii=False)}\n\n".encode()
            self.wfile.write(chunk)
            self.wfile.flush()

        frame("ready", {})
        frame("thinking", {"reasoning_content": "想一下"})
        for piece in pieces:
            frame("message_chunk", {"content": piece})
        frame("message_finish", {})
        self.close_connection = True


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
