#!/usr/bin/env python3
"""No paid upstreams: check acceptance parsing, decimal accounting and secret handling."""
import contextlib
from argparse import Namespace
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

spec = importlib.util.spec_from_file_location("manual", Path(__file__).with_name("manual-test.py"))
manual = importlib.util.module_from_spec(spec)
spec.loader.exec_module(manual)


def rejected(operation):
    try:
        operation()
    except RuntimeError:
        return
    raise AssertionError("invalid result was accepted")


def main():
    chat = {"choices": [{"message": {"content": "OK"}, "finish_reason": "stop"}], "usage": {"total_tokens": 3}}
    manual.generation_ok(json.dumps(chat), "chat", False)
    chat["choices"][0]["finish_reason"] = "length"
    rejected(lambda: manual.generation_ok(json.dumps(chat), "chat", False))
    stream = 'data: {"choices":[{"delta":{"content":"OK"}}]}\n\ndata: {"usage":{"total_tokens":3}}\n\ndata: [DONE]\n\n'
    manual.generation_ok(stream, "chat", True)
    rejected(lambda: manual.generation_ok(stream.replace("data: [DONE]\n\n", ""), "chat", True))
    rejected(lambda: manual.generation_ok(stream + 'data: {"error":{"message":"late failure"}}\n\n', "chat", True))
    response = {"status": "completed", "usage": {"total_tokens": 3},
                "output": [{"content": [{"type": "output_text", "text": "OK"}]}]}
    manual.generation_ok(json.dumps(response), "responses", False)
    completed = "event: response.completed\ndata: " + json.dumps({"type": "response.completed", "response": response}) + "\n\n"
    manual.generation_ok(completed, "responses", True)
    rejected(lambda: manual.generation_ok(completed.replace("completed", "incomplete"), "responses", True))
    response.pop("usage")
    rejected(lambda: manual.generation_ok(json.dumps(response), "responses", False))
    response["usage"] = {"total_tokens": 3}
    chat["choices"][0]["finish_reason"] = "stop"

    usage = [{"api_key_id": 2, "actual_cost": "0.000000015"}, {"api_key_id": 2, "actual_cost": "0.000000015"}]
    keys = {"chat": {"id": 2, "quota_used": "0.00000004"}}
    manual.reconcile({"balance": "9.99999996"}, keys, usage, [{"value": "10"}])
    rejected(lambda: manual.reconcile({"balance": "10"}, keys, usage, [{"value": "10"}]))

    class Handler(BaseHTTPRequestHandler):
        calls = []

        def log_message(self, *args):
            pass

        def do_GET(self):
            self.calls.append(self.path)
            if self.path == "/redirect":
                self.send_response(302)
                self.send_header("Location", "/should-not-follow")
                self.end_headers()
                return
            self.send_response(403)
            self.end_headers()
            self.wfile.write(json.dumps({"message": "secret-from-config rejected", "key": "returned-secret"}).encode())

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if body.get("stream"):
                data = stream if self.path == "/v1/chat/completions" else completed
                content_type = "text/event-stream"
            else:
                data = json.dumps(chat if self.path == "/v1/chat/completions" else response)
                content_type = "application/json"
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.end_headers()
            self.wfile.write(data.encode())

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        with tempfile.TemporaryDirectory() as temporary, contextlib.redirect_stdout(io.StringIO()) as captured:
            directory = Path(temporary)
            manual.prepare(directory)
            config = manual.load_json(directory / "config.json")
            original = dict(config)
            manual.prepare(directory)
            assert manual.load_json(directory / "config.json") == original
            config.update(base_url=f"http://127.0.0.1:{server.server_port}", upstream_api_key="secret-from-config")
            manual.write_private(directory / "config.json", config)
            app = manual.Manual(directory)
            assert app.request("GET", "/redirect", "bearer-secret")[0] == 302
            assert Handler.calls == ["/redirect"]
            assert app.request("GET", "/failure", "bearer-secret")[0] == 403
            app.state = {label: {"key": "bearer-secret"} for label in manual.PROTOCOLS}
            for label in manual.PROTOCOLS:
                for streaming in (False, True):
                    app.generate(Namespace(command=label, stream=streaming, prompt="Reply only OK.", idempotency_key="test"))
            for path in (directory / "results").glob("*.json"):
                raw = path.read_text()
                assert not any(secret in raw for secret in ("secret-from-config", "bearer-secret", "returned-secret"))
                assert path.stat().st_mode & 0o777 == 0o600
            assert "secret-from-config" not in captured.getvalue()
            config["base_url"] = "https://example.com"
            manual.write_private(directory / "config.json", config)
            rejected(lambda: manual.Manual(directory))
    finally:
        server.shutdown()
        server.server_close()
        worker.join()
    print("PASS: terminal events, incomplete/error rejection, decimal rounding, redirects, redaction and config preservation")


if __name__ == "__main__":
    main()
