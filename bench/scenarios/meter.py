#!/usr/bin/env python3
"""A metering pass-through for the Anthropic API.

spore's provider is pointed at http://127.0.0.1:<port>; every request is
forwarded unchanged to api.anthropic.com and the response is streamed back as
it arrives. The usage Anthropic reports for each call (message_start and
message_delta events) is appended to a JSON-lines log, so a run's cost is
measured from the provider's own figures, for every call site: chat,
titles, compaction, refinement, sub-agents.

Usage: meter.py <port> <log file>
"""
import http.client, http.server, json, ssl, sys

PORT, LOG = int(sys.argv[1]), sys.argv[2]
UPSTREAM = "api.anthropic.com"
HOP = {"host", "content-length", "connection", "accept-encoding", "transfer-encoding"}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        model = ""
        try:
            model = json.loads(body).get("model", "")
        except ValueError:
            pass
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
        conn = http.client.HTTPSConnection(UPSTREAM, context=ssl.create_default_context(), timeout=600)
        conn.request("POST", self.path, body=body, headers=headers)
        resp = conn.getresponse()
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in HOP:
                self.send_header(k, v)
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        usage, buf = {}, b""
        while True:
            chunk = resp.read1(65536)
            if not chunk:
                break
            self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
            self.wfile.flush()
            buf += chunk
            *lines, buf = buf.split(b"\n")
            for line in lines:
                if not line.startswith(b"data: "):
                    continue
                try:
                    ev = json.loads(line[6:])
                except ValueError:
                    continue
                u = (ev.get("message") or {}).get("usage") if ev.get("type") == "message_start" else ev.get("usage")
                if u:
                    usage.update({k: v for k, v in u.items() if isinstance(v, int)})
        self.wfile.write(b"0\r\n\r\n")
        with open(LOG, "a") as f:
            f.write(json.dumps({"status": resp.status, "model": model, "usage": usage}) + "\n")


http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
