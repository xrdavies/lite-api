#!/usr/bin/env python3
"""Exercise release images, persistent recovery and rollback in disposable Compose volumes."""
from concurrent.futures import ThreadPoolExecutor
from decimal import Decimal
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import ipaddress
import json
import os
from pathlib import Path
import secrets
import socket
import statistics
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request

root = Path(__file__).resolve().parent.parent
docker = ["docker"]
if os.getenv("DOCKER_CONTEXT"):
    docker += ["--context", os.environ["DOCKER_CONTEXT"]]
project = "lite-api-release-test-" + secrets.token_hex(6)
images = [project + ":current", project + ":rollback"]
env = os.environ.copy()
env.update(POSTGRES_PASSWORD=secrets.token_hex(24), JWT_SECRET=secrets.token_hex(32),
           ADMIN_EMAIL="admin@example.test", ADMIN_PASSWORD=secrets.token_hex(16),
           LITE_API_IMAGE=images[0], LITE_API_VERSION="deployment-current")
provider_key = secrets.token_hex(24)
calls = {"text": 0, "video": 0, "program": 0}
calls_lock = threading.Lock()
video_ready = threading.Event()
program_ready = threading.Event()
secrets_seen = [env[k] for k in ("POSTGRES_PASSWORD", "JWT_SECRET", "ADMIN_PASSWORD")] + [provider_key]


def run(args, *, check=True, **kwargs):
    result = subprocess.run(args, cwd=root, env=env, capture_output=True, text=True, **kwargs)
    if check and result.returncode:
        error = result.stderr[-3000:]
        for value in secrets_seen:
            error = error.replace(value, "[redacted]")
        raise RuntimeError(f"{args[0]} failed ({result.returncode}): {error}")
    return result


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert self.headers.get("Authorization") == "Bearer " + provider_key
        assert self.headers.get("Cookie") is None
        if self.path == "/api/v3/contents/generations/tasks":
            assert body["model"] == "deploy-video"
            with calls_lock:
                calls["video"] += 1
            raw = b'{"id":"native_video"}'
            content_type = "application/json"
        elif self.path == "/v1/responses":
            assert body["model"] == "deploy-program"
            with calls_lock:
                calls["program"] += 1
            if body.get("background"):
                assert body["tools"] == [{"type": "programmatic_tool_calling"}]
                result = {"id": "resp_deploy_program", "object": "response", "status": "queued"}
            else:
                assert body["previous_response_id"] == "resp_deploy_program" and "tools" not in body
                result = {"id": "resp_deploy_continued", "object": "response", "model": "deploy-program",
                          "status": "completed", "output": [], "usage": {"input_tokens": 2, "output_tokens": 3}}
            raw = json.dumps(result).encode()
            content_type = "application/json"
        else:
            assert self.path == "/v1/chat/completions" and body["model"] == "deploy-text"
            with calls_lock:
                calls["text"] += 1
            response = {"id": "deploy_chat", "model": "deploy-text", "object": "chat.completion",
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}}
            content_type = "application/json"
            if body.get("stream"):
                response["object"] = "chat.completion.chunk"
                response["choices"][0]["delta"] = response["choices"][0].pop("message")
                raw = ("data: " + json.dumps(response) + "\n\ndata: [DONE]\n\n").encode()
                content_type = "text/event-stream"
            else:
                raw = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        assert self.headers.get("Authorization") == "Bearer " + provider_key
        if self.path == "/v1/responses/resp_deploy_program":
            result = {"id": "resp_deploy_program", "object": "response", "status": "in_progress", "model": "deploy-program"}
            if program_ready.is_set():
                result.update(status="completed", usage={"input_tokens": 2, "output_tokens": 3}, output=[
                    {"type": "program", "id": "prog_deploy", "call_id": "pc_deploy", "code": "return 42;", "fingerprint": "deploy-replay"},
                    {"type": "program_output", "id": "po_deploy", "call_id": "pc_deploy", "result": "42", "status": "completed"}])
        else:
            assert self.path == "/api/v3/contents/generations/tasks/native_video"
            result = {"id": "native_video", "status": "running", "model": "deploy-video"}
            if video_ready.is_set():
                result.update(status="succeeded", content={"video_url": "https://example.test/video.mp4"},
                              usage={"completion_tokens": 11, "total_tokens": 11})
        raw = json.dumps(result).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def request(base, method, path, token="", body=None, idem=""):
    headers = {"Content-Type": "application/json", "Authorization": "Bearer " + token}
    if idem:
        headers["Idempotency-Key"] = idem
    req = urllib.request.Request(base + path, method=method, headers=headers,
                                 data=None if body is None else json.dumps(body).encode())
    try:
        response = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=45)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.read(), response.headers


def eventually(check, label, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.5)
    raise AssertionError("timed out: " + label)


provider = ThreadingHTTPServer(("0.0.0.0", 0), Provider)
threading.Thread(target=provider.serve_forever, daemon=True).start()
with socket.socket() as port:
    port.bind(("127.0.0.1", 0))
    env["LITE_API_PORT"] = str(port.getsockname()[1])
base = "http://127.0.0.1:" + env["LITE_API_PORT"]

with tempfile.TemporaryDirectory(prefix=project) as temp:
    temp = Path(temp)
    empty_env = temp / "empty.env"
    empty_env.touch()
    override = temp / "override.json"
    override.write_text(json.dumps({"services": {"app": {"extra_hosts": ["host.docker.internal:host-gateway"]}}}))
    compose = docker + ["compose", "--env-file", str(empty_env), "--project-name", project,
                        "-f", str(root / "compose.deploy.yaml"), "-f", str(override)]

    def dc(*args, **kwargs):
        return run(compose + list(args), **kwargs)

    def sql(query):
        return dc("exec", "-T", "postgres", "psql", "-XAt", "-v", "ON_ERROR_STOP=1", "-U", "lite_api", "-d", "lite_api", input=query).stdout.strip()

    def redis(*args):
        return dc("exec", "-T", "redis", "redis-cli", "--raw", *args).stdout.strip()

    def ready():
        def check():
            try:
                return request(base, "GET", "/health")[0] == 200
            except (OSError, urllib.error.URLError):
                return False
        eventually(check, "application health")

    def api(method, path, token="", body=None, idem=""):
        status, raw, _ = request(base, method, path, token, body, idem)
        assert status in (200, 201), f"{method} {path}: HTTP {status}"
        return json.loads(raw)["data"]

    def kill(service):
        container = dc("ps", "-q", service).stdout.strip()
        assert container
        run(docker + ["update", "--restart=no", container])
        run(docker + ["kill", "--signal=KILL", container])

    try:
        print("Building current and previous committed release images", flush=True)
        pg_image = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"
        hosts = run(docker + ["run", "--rm", "--add-host", "host.docker.internal:host-gateway", "--entrypoint", "getent", pg_image, "hosts", "host.docker.internal"]).stdout
        address = ipaddress.ip_address(hosts.split()[0])
        env["UPSTREAM_PRIVATE_CIDRS"] = str(address) + ("/32" if address.version == 4 else "/128")
        dc("build", "app")
        archive = subprocess.check_output(["git", "archive", "HEAD^"], cwd=root)
        previous = temp / "previous"
        previous.mkdir()
        with tarfile.open(fileobj=io.BytesIO(archive)) as snapshot:
            snapshot.extractall(previous, filter="data")
        run(docker + ["build", "--build-arg", "VERSION=deployment-rollback", "-t", images[1], str(previous)])
        dc("up", "-d", "--wait", "postgres", "redis")
        dc("run", "--rm", "--no-deps", "app", "init-db")
        dc("run", "--rm", "--no-deps", "-e", "ADMIN_EMAIL", "-e", "ADMIN_PASSWORD", "app", "bootstrap")
        assert dc("run", "--rm", "--no-deps", "app", "init-db", check=False).returncode != 0
        dc("up", "-d", "--no-build", "app")
        ready()
        container = dc("ps", "-q", "app").stdout.strip()
        config = json.loads(run(docker + ["inspect", container]).stdout)[0]
        assert config["Config"]["User"] == "65532:65532" and config["HostConfig"]["ReadonlyRootfs"]
        dc("exec", "-T", "app", "/lite-api", "healthcheck")
        duplicate = dc("run", "--rm", "--no-deps", "app", "serve", check=False, timeout=45)
        assert duplicate.returncode != 0 and "another lite-api instance" in duplicate.stderr
        admin = api("POST", "/api/v1/auth/login", body={"email": env["ADMIN_EMAIL"], "password": env["ADMIN_PASSWORD"]})["access_token"]
        password = secrets.token_hex(16)
        user = api("POST", "/api/v1/admin/users", admin, {"email": "user@example.test", "password": password, "balance": 10, "concurrency": 16})
        uid = user["id"]
        token = api("POST", "/api/v1/auth/login", body={"email": "user@example.test", "password": password})["access_token"]
        group = api("POST", "/api/v1/admin/groups", admin, {"name": "Deployment test", "platform": "openai", "rate_multiplier": 2,
                    "model_pricing": [{"platform": "openai", "models": ["deploy-text", "deploy-video"], "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}]})
        gid = group["id"]
        upstream = "http://host.docker.internal:" + str(provider.server_port)
        for model in ("deploy-text", "deploy-video"):
            credentials = {"api_key": provider_key, "base_url": upstream, "model_mapping": {model: model}, "api_protocol": "chat_completions"}
            if model == "deploy-video":
                credentials["openai_capabilities"] = ["seedance"]
            api("POST", "/api/v1/admin/accounts", admin, {"name": model, "platform": "openai", "type": "apikey", "group_ids": [gid], "concurrency": 16,
                "rate_multiplier": 3, "extra": {"quota_limit": 100}, "credentials": credentials})
        key_object = api("POST", "/api/v1/keys", token, {"name": "Deployment test", "group_id": gid, "quota": 100})
        key, kid = key_object["key"], key_object["id"]
        secrets_seen += [admin, token, password, key]
        body = {"model": "deploy-text", "messages": [{"role": "user", "content": "OK"}]}
        status, first, _ = request(base, "POST", "/v1/chat/completions", key, body, "deploy-json")
        if status != 200:
            failure = first.decode(errors="replace")
            for secret in secrets_seen:
                failure = failure.replace(secret, "[redacted]")
            raise AssertionError(f"initial gateway HTTP {status}: {failure}")
        assert json.loads(first)["choices"][0]["message"]["content"] == "OK"
        streamed = dict(body, stream=True)
        status, raw, _ = request(base, "POST", "/v1/chat/completions", key, streamed, "deploy-stream")
        assert status == 200 and b"data: [DONE]" in raw
        replay = request(base, "POST", "/chat/completions", key, body, "deploy-json")
        assert replay[1] == first and replay[2].get("Idempotency-Replayed") == "true" and calls["text"] == 2
        api("POST", f"/api/v1/admin/users/{uid}/balance", admin, {"balance": 1, "operation": "add"}, "deploy-balance")
        print("Initialized; JSON/SSE, identity, balances and idempotency verified", flush=True)

        def timed(base_url, credential):
            start = time.perf_counter()
            status, data, _ = request(base_url, "POST", "/v1/chat/completions", credential, body)
            assert status == 200 and json.loads(data)["usage"]["total_tokens"] == 5
            return (time.perf_counter() - start) * 1000

        direct = [timed("http://127.0.0.1:" + str(provider.server_port), provider_key) for _ in range(20)]
        with ThreadPoolExecutor(max_workers=8) as pool:
            latency = list(pool.map(lambda _: timed(base, key), range(40)))
        p95 = sorted(latency)[37]
        stats = json.loads(run(docker + ["stats", "--no-stream", "--format", "{{json .}}", container]).stdout)
        print(json.dumps({"concurrency": 8, "requests": 40, "direct_p50_ms": round(statistics.median(direct), 2),
                          "gateway_p50_ms": round(statistics.median(latency), 2), "gateway_p95_ms": round(p95, 2),
                          "memory": stats["MemUsage"]}), flush=True)
        status, raw, _ = request(base, "POST", "/v1/contents/generations/tasks", key,
                                {"model": "deploy-video", "content": [{"type": "text", "text": "test"}]}, "deploy-video")
        assert status == 200
        task_id = json.loads(raw)["id"]
        count_before = int(sql("SELECT count(*) FROM usage_logs"))
        assert count_before == 42
        sql("ALTER TABLE usage_logs ADD CONSTRAINT deployment_failure CHECK (false) NOT VALID;")
        status, raw, _ = request(base, "POST", "/v1/chat/completions", key, streamed, "deploy-recover")
        assert b"settlement" in raw and b"data: [DONE]" not in raw
        assert redis("HLEN", "gateway:pending-billing") == "1"
        assert int(sql("SELECT count(*) FROM usage_logs")) == count_before
        before_recovery = dict(calls)
        kill("app")
        kill("redis")
        kill("postgres")
        dc("up", "-d", "--wait", "postgres", "redis")
        assert redis("HLEN", "gateway:pending-billing") == "1"
        assert redis("SCARD", "gateway:video:pending") == "1"
        assert int(sql("SELECT count(*) FROM usage_logs")) == count_before
        sql("ALTER TABLE usage_logs DROP CONSTRAINT deployment_failure;")
        video_ready.set()
        env["LITE_API_IMAGE"] = images[1]
        dc("up", "-d", "--no-build", "--force-recreate", "app")
        ready()
        assert api("GET", "/api/v1/admin/system/version", admin)["version"] == "deployment-rollback"
        eventually(lambda: redis("HLEN", "gateway:pending-billing") == "0" and redis("SCARD", "gateway:video:pending") == "0", "rollback recovery")
        assert calls == before_recovery, "recovery repeated upstream creation"
        assert int(sql("SELECT count(*) FROM usage_logs")) == count_before + 2
        status, raw, _ = request(base, "GET", "/api/v3/contents/generations/tasks/" + task_id, key)
        assert status == 200 and json.loads(raw)["status"] == "succeeded"
        assert request(base, "POST", "/chat/completions", key, body, "deploy-json")[1] == first
        api("POST", f"/api/v1/admin/users/{uid}/balance", admin, {"balance": 1, "operation": "add"}, "deploy-balance")
        expected = Decimal("0.016") * 43 + Decimal("0.044")
        wallet, quota, usage = sql(f"SELECT u.balance,k.quota_used,(SELECT sum(actual_cost) FROM usage_logs) FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id={kid}").split("|")
        assert Decimal(wallet) == 11 - expected and Decimal(quota) == expected and Decimal(usage) == expected
        assert sql("SELECT bool_and(n=1) FROM (SELECT count(*) n FROM usage_logs GROUP BY request_id,api_key_id)s") == "t"
        print("SIGKILL of app/Redis/PostgreSQL: ledger, AOF, sessions, video and previous-release recovery verified", flush=True)
        dc("stop", "app")
        env["LITE_API_IMAGE"] = images[0]
        dc("up", "-d", "--no-build", "--force-recreate", "app")
        ready()
        assert api("GET", "/api/v1/admin/system/version", admin)["version"] == "deployment-current"
        assert int(sql("SELECT count(*) FROM usage_logs")) == count_before + 2
        assert request(base, "POST", "/chat/completions", key, body, "deploy-json")[1] == first and calls == before_recovery
        # New execution metadata must recover on a version that understands it.
        # Exercise this only after rollback/re-upgrade; never give the old image
        # a programmatic session whose safeguards it cannot recognize.
        pgid = api("POST", "/api/v1/admin/groups", admin, {"name": "Program recovery", "platform": "openai", "rate_multiplier": 2,
                    "model_pricing": [{"platform": "openai", "models": ["deploy-program"], "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}]})["id"]
        api("POST", "/api/v1/admin/accounts", admin, {"name": "Program provider", "platform": "openai", "type": "apikey", "group_ids": [pgid],
            "credentials": {"api_key": provider_key, "base_url": upstream, "api_protocol": "responses"}})
        pkey_obj = api("POST", "/api/v1/keys", token, {"name": "Program recovery", "group_id": pgid, "quota": 100})
        pkey, pkid = pkey_obj["key"], pkey_obj["id"]
        secrets_seen.append(pkey)
        program = {"model": "deploy-program", "input": "calculate", "tools": [{"type": "programmatic_tool_calling"}], "store": True, "background": True}
        status, raw, _ = request(base, "POST", "/responses", pkey, program, "deploy-program")
        if status != 200:
            error = raw.decode(errors="replace")
            for value in secrets_seen:
                error = error.replace(value, "[redacted]")
            raise AssertionError(f"program submission HTTP {status}: {error}")
        assert json.loads(raw)["id"] == "resp_deploy_program"
        api("PUT", f"/api/v1/admin/groups/{pgid}", admin, {"rate_multiplier": 9})
        sql("ALTER TABLE usage_logs ADD CONSTRAINT program_failure CHECK (false) NOT VALID;")
        program_ready.set()
        status, raw, _ = request(base, "GET", "/responses/resp_deploy_program", pkey)
        assert status in (409, 503) and b'"status":"completed"' not in raw
        assert redis("SCARD", "gateway:background:pending") == "1"
        before_program = dict(calls)
        kill("app")
        kill("redis")
        kill("postgres")
        dc("up", "-d", "--wait", "postgres", "redis")
        assert redis("SCARD", "gateway:background:pending") == "1"
        sql("ALTER TABLE usage_logs DROP CONSTRAINT program_failure;")
        dc("up", "-d", "--no-build", "--force-recreate", "app")
        ready()
        eventually(lambda: redis("SCARD", "gateway:background:pending") == "0", "program settlement recovery")
        assert calls == before_program, "program recovery repeated creation"
        for _ in range(2):
            status, raw, _ = request(base, "GET", "/responses/resp_deploy_program", pkey)
            result = json.loads(raw)
            assert status == 200 and result["status"] == "completed"
            assert result["output"][0]["fingerprint"] == "deploy-replay" and result["output"][1]["result"] == "42"
        assert request(base, "GET", "/responses/resp_deploy_program", key)[0] == 404
        count, cost, used = sql(f"SELECT (SELECT count(*) FROM usage_logs WHERE api_key_id={pkid}),(SELECT sum(actual_cost) FROM usage_logs WHERE api_key_id={pkid}),quota_used FROM api_keys WHERE id={pkid}").split("|")
        assert count == "1" and Decimal(cost) == Decimal("0.016") and Decimal(used) == Decimal(cost)
        assert Decimal(sql(f"SELECT balance FROM users WHERE id={uid}")) == 11 - expected - Decimal("0.016")
        status, _, headers = request(base, "POST", "/v1/responses", pkey, program, "deploy-program")
        assert status == 200 and headers.get("Idempotency-Replayed") == "true" and calls == before_program
        continued = {"model": "deploy-program", "input": "continue", "previous_response_id": "resp_deploy_program"}
        assert request(base, "POST", "/responses", key, continued)[0] == 404
        status, raw, _ = request(base, "POST", "/responses", pkey, continued)
        assert status == 200 and json.loads(raw)["id"] == "resp_deploy_continued"
        assert calls["program"] == before_program["program"] + 1
        cost = sql(f"SELECT sum(actual_cost) FROM usage_logs WHERE api_key_id={pkid}")
        assert Decimal(cost) == Decimal("0.016") + Decimal("0.072")
        assert Decimal(sql(f"SELECT balance FROM users WHERE id={uid}")) == 11 - expected - Decimal(cost)
        print("Programmatic task: SIGKILL recovery, original price, one settlement, replay and scoped continuation verified", flush=True)
        # Exercise compiled-in token vocabularies in the scratch image. Both
        # local count paths must leave the provider, wallet and ledger untouched.
        before_counts = dict(calls)
        ledger = sql(f"SELECT balance,(SELECT count(*) FROM usage_logs) FROM users WHERE id={uid}")
        native_count = {"model": "deploy-text", "input": "Hello 世界"}
        status, first_count, _ = request(base, "POST", "/v1/responses/input_tokens", key, native_count, "release-native-count")
        assert status == 200 and json.loads(first_count)["object"] == "response.input_tokens" and json.loads(first_count)["input_tokens"] > 0
        status, repeated, headers = request(base, "POST", "/responses/input_tokens", key, native_count, "release-native-count")
        assert status == 200 and repeated == first_count and headers.get("Idempotency-Replayed") == "true"
        for platform in ("grok", "deepseek"):
            cg = api("POST", "/api/v1/admin/groups", admin, {"name": "Count " + platform, "platform": platform})["id"]
            if platform == "deepseek":
                api("POST", "/api/v1/admin/accounts", admin, {"name": "Count supplier", "platform": platform,
                    "type": "apikey", "group_ids": [cg], "credentials": {"api_key": provider_key, "base_url": upstream}})
            ck = api("POST", "/api/v1/keys", token, {"name": "Count " + platform, "group_id": cg})["key"]
            secrets_seen.append(ck)
            count_body = {"model": "local-count", "messages": [{"role": "user", "content": "Hello 世界"}]}
            status, first_count, _ = request(base, "POST", "/v1/messages/count_tokens", ck, count_body, "release-count")
            assert status == 200 and json.loads(first_count)["input_tokens"] > 0
            status, repeated, headers = request(base, "POST", "/messages/count_tokens", ck, count_body, "release-count")
            assert status == 200 and repeated == first_count and headers.get("Idempotency-Replayed") == "true"
        assert calls == before_counts, "local counting contacted the provider"
        assert sql(f"SELECT balance,(SELECT count(*) FROM usage_logs) FROM users WHERE id={uid}") == ledger
        print("Compiled token vocabularies: native relay, Grok without account and DeepSeek local counts, replay and zero billing verified", flush=True)
        logs = dc("logs", "--no-color", "app").stdout
        assert not any(secret in logs for secret in secrets_seen), "secret in application logs"
        print("PASS: isolated release, crash recovery, rollback and upgrade; no paid upstream calls", flush=True)
    finally:
        dc("down", "--volumes", "--remove-orphans", check=False)
        run(docker + ["image", "rm", *images], check=False)
        provider.shutdown()
        provider.server_close()
