#!/usr/bin/env python3
"""A tiny fake Telegram Bot API for test/units/first-run.sh.

It listens on 127.0.0.1:PORT. It answers every method with "ok", gives no
updates, and appends the name of each method it gets to LOG, one per line.
It never prints a request, so the test token never shows.

Usage: python3 -I faketg.py PORT LOG
"""

import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
LOG = sys.argv[2]
CHAT = 4242


class Handler(BaseHTTPRequestHandler):
    next_id = 1

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        method = self.path.rsplit("/", 1)[-1]
        with open(LOG, "a", encoding="utf-8") as f:
            f.write(method + "\n")
        if method == "getUpdates":
            time.sleep(1)
            result = []
        elif method == "sendMessage":
            result = {"message_id": Handler.next_id, "chat": {"id": CHAT}, "text": ""}
            Handler.next_id += 1
        else:
            result = True
        body = json.dumps({"ok": True, "result": result}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
