#!/usr/bin/env python3
"""A recording proxy in front of the S3 gateway, for the host phase of the itest.

The agent sends every request through it. For each request it appends one JSON
line to the log: the time, the method, the path, whether the request carried a
Content-MD5 header and whether that header matches the body, and what the
gateway answered. It forwards the request unchanged (the Host header too, which
the signature covers), and it can refuse requests instead of forwarding them,
which is how the itest makes an upload fail and be retried.

  s3-recorder.py LISTEN UPSTREAM LOG CONTROL_DIR      (LISTEN, UPSTREAM: host:port)

CONTROL_DIR is read on every request:
  fail-all       exists: every request is answered 503 and not forwarded
  fail-next      holds a number N: the next N requests are answered 503 and
                 not forwarded, and the number counts down

Only the Python standard library.
"""
import base64
import hashlib
import http.client
import http.server
import json
import os
import socketserver
import sys
import threading
import time

HOP = {"connection", "keep-alive", "proxy-connection", "transfer-encoding", "te", "trailer", "upgrade"}
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # the JSON log is the record
        pass

    def read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            out = b""
            while True:
                size = int(self.rfile.readline().split(b";")[0], 16)
                if size == 0:
                    self.rfile.readline()
                    return out
                out += self.rfile.read(size)
                self.rfile.readline()
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def refuse(self):
        """True when the control directory says to refuse this request."""
        ctl = self.server.control
        if os.path.exists(os.path.join(ctl, "fail-all")):
            return True
        path = os.path.join(ctl, "fail-next")
        with lock:
            try:
                n = int(open(path).read().strip() or 0)
            except (OSError, ValueError):
                return False
            if n <= 0:
                return False
            with open(path, "w") as f:
                f.write(str(n - 1))
            return True

    def handle_any(self):
        body = self.read_body()
        entry = {"t": round(time.time(), 3), "method": self.command, "path": self.path, "bytes": len(body)}
        want = self.headers.get("Content-MD5")
        entry["md5_header"] = want is not None
        if want is not None:
            entry["md5_ok"] = want == base64.b64encode(hashlib.md5(body).digest()).decode()
        if self.refuse():
            entry.update(status=503, forwarded=False, injected=True)
            payload = b'<?xml version="1.0"?><Error><Code>ServiceUnavailable</Code><Message>refused by the test</Message></Error>'
            self.send_response(503)
            self.send_header("Content-Type", "application/xml")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        else:
            host, _, port = self.server.upstream.partition(":")
            up = http.client.HTTPConnection(host, int(port), timeout=60)
            up.putrequest(self.command, self.path, skip_host=True, skip_accept_encoding=True)
            for k, v in self.headers.items():
                if k.lower() not in HOP and k.lower() != "content-length":
                    up.putheader(k, v)
            up.putheader("Content-Length", str(len(body)))
            up.endheaders(body)
            resp = up.getresponse()
            payload = resp.read()
            entry.update(status=resp.status, forwarded=True, injected=False)
            self.send_response(resp.status)
            for k, v in resp.getheaders():
                if k.lower() not in HOP and k.lower() != "content-length":
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(payload)
            up.close()
        with lock, open(self.server.log, "a") as f:
            f.write(json.dumps(entry) + "\n")

    do_GET = do_PUT = do_POST = do_DELETE = do_HEAD = handle_any


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def main():
    listen, upstream, log, control = sys.argv[1:5]
    host, _, port = listen.partition(":")
    srv = Server((host, int(port)), Handler)
    srv.upstream, srv.log, srv.control = upstream, log, control
    os.makedirs(control, exist_ok=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
