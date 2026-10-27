#!/usr/bin/env python3
"""Deterministic OpenAI-compatible provider for isolated OpenCode smoke tests.

Never configure this provider in a normal user workspace: it returns a fixed or
requested marker and does not perform model inference. Optional request logs
contain test prompts and image bytes, so use a disposable output directory.
"""
import argparse
import json
import re
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--port", type=int, default=14097)
parser.add_argument("--request-log")
args = parser.parse_args()


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if args.request_log:
            with open(args.request_log, "a") as output:
                output.write(json.dumps(body) + "\n")
        texts = []
        for message in body.get("messages", []):
            if message.get("role") != "user":
                continue
            content = message.get("content", "")
            texts.append(content if isinstance(content, str) else " ".join(part.get("text", "") for part in content))
        match = re.search(r"Reply (?:with )?exactly (\w+)", texts[-1] if texts else "", re.I)
        reply = match.group(1) if match else "PRISM_V2_OK"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
        self.end_headers()
        if not body.get("stream"):
            result = {"id": "smoke", "object": "chat.completion", "choices": [{"index": 0, "message": {"role": "assistant", "content": reply}, "finish_reason": "stop"}]}
            self.wfile.write(json.dumps(result).encode())
            return
        # Leave enough time for a real client to subscribe or interrupt.
        time.sleep(1)
        try:
            for text in [reply[:6], reply[6:12], reply[12:]]:
                chunk = {"id": "smoke", "object": "chat.completion.chunk", "created": int(time.time()), "model": "smoke", "choices": [{"index": 0, "delta": {"role": "assistant", "content": text}, "finish_reason": None}]}
                self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
                self.wfile.flush()
                time.sleep(0.15)
            chunk["choices"] = [{"index": 0, "delta": {}, "finish_reason": "stop"}]
            chunk["usage"] = {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode())
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *_):
        pass


ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
