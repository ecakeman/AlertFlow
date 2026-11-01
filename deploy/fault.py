"""最小故障目标：GET /health 在收到 POST /recover 之前返回 500。"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os

FLAG = os.environ.get("FAULT_FLAG", "/tmp/alertflow-healthy")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        healthy = os.path.exists(FLAG)
        code = 200 if healthy else 500
        body = b"ok" if healthy else b"down"
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path == "/recover":
            open(FLAG, "w").close()
        elif self.path == "/break" and os.path.exists(FLAG):
            os.remove(FLAG)
        self.send_response(204)
        self.end_headers()

    def log_message(self, fmt, *args):
        return


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8081"))
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
