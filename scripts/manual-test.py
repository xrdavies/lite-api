#!/usr/bin/env python3
"""Local, step-by-step HTTP acceptance helpers. Python standard library only."""
import argparse
from datetime import datetime, timezone
from decimal import Decimal, ROUND_HALF_UP
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parent.parent
SECRET_FIELDS = {"password", "admin_password", "user_password", "jwt_secret", "postgres_password",
                 "api_key", "upstream_api_key", "key", "access_token", "refresh_token", "authorization"}
PROTOCOLS = {"chat": "chat_completions", "responses": "responses"}
PRICES = {"input_price": "0.000001", "output_price": "0.000002",
          "cache_read_price": "0.0000005", "cache_write_price": "0.000001"}


def write_private(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_suffix(path.suffix + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(value, f, ensure_ascii=False, indent=2, default=str)
        f.write("\n")
    os.chmod(temporary, 0o600)
    temporary.replace(path)


def load_json(path):
    return json.loads(path.read_text())


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def generation_ok(raw, protocol, stream):
    """Require meaningful output, usage and a successful terminal event."""
    text, usage, complete, failed = "", False, False, False
    if stream:
        entries = []
        for frame in raw.replace("\r\n", "\n").split("\n\n"):
            data = "\n".join(line[5:].lstrip() for line in frame.splitlines() if line.startswith("data:"))
            if data == "[DONE]":
                complete = protocol == "chat"
            elif data:
                entries.append(json.loads(data))
    else:
        entries = [json.loads(raw)]
    for item in entries:
        failed |= bool(item.get("error")) or item.get("type") in {"error", "response.failed", "response.incomplete"}
        if protocol == "chat":
            usage |= isinstance(item.get("usage"), dict) and bool(item["usage"])
            for choice in item.get("choices", []):
                text += (choice.get("delta" if stream else "message", {}).get("content") or "")
                failed |= choice.get("finish_reason") in {"length", "content_filter"}
                if not stream:
                    complete |= choice.get("finish_reason") == "stop"
        else:
            if item.get("type") == "response.output_text.delta":
                text += item.get("delta", "")
            response = item.get("response", {}) if stream else item
            usage |= isinstance(response.get("usage"), dict) and bool(response["usage"])
            complete |= response.get("status") == "completed" and (not stream or item.get("type") == "response.completed")
            failed |= response.get("status") in {"failed", "incomplete", "cancelled"}
            for output in response.get("output", []):
                text += "".join(part.get("text", "") for part in output.get("content", []) if part.get("type") == "output_text")
    require(complete and usage and text.strip() and not failed,
            "生成未完整通过：需文本、usage 和成功终态；检查结果文件中的错误或输出上限。")


class Manual:
    def __init__(self, directory):
        self.directory = directory
        self.config = load_json(directory / "config.json")
        self.state = load_json(directory / "state.json") if (directory / "state.json").exists() else {}
        self.secrets = set()
        self.remember(self.config)
        self.remember(self.state)
        url = urllib.parse.urlsplit(self.config["base_url"])
        require(url.scheme == "http" and url.hostname == "127.0.0.1" and url.port
                and not url.username and not url.password and url.path in {"", "/"} and not url.query and not url.fragment,
                "base_url 必须是 http://127.0.0.1:端口；此脚本只用于本地人工测试。")
        self.base = self.config["base_url"].rstrip("/")
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def remember(self, value):
        if isinstance(value, dict):
            for key, item in value.items():
                if key.lower() in SECRET_FIELDS and isinstance(item, str) and item:
                    self.secrets.add(item)
                self.remember(item)
        elif isinstance(value, list):
            for item in value:
                self.remember(item)

    def clean(self, value):
        if isinstance(value, dict):
            return {k: "[redacted]" if k.lower() in SECRET_FIELDS else self.clean(v) for k, v in value.items()}
        if isinstance(value, list):
            return [self.clean(v) for v in value]
        if isinstance(value, str):
            for secret in sorted(self.secrets, key=len, reverse=True):
                value = value.replace(secret, "[redacted]")
        return value

    def save(self):
        write_private(self.directory / "state.json", self.state)

    def request(self, method, path, token="", body=None, idem="", stream=False):
        require(path.startswith("/") and not path.startswith("//"), "请求路径必须以单个 / 开头。")
        self.remember(body)
        if token:
            self.secrets.add(token)
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = "Bearer " + token
        if idem:
            headers["Idempotency-Key"] = idem
        payload = None if body is None else json.dumps(body, default=str).encode()
        request = urllib.request.Request(self.base + path, data=payload, headers=headers, method=method)
        started = time.monotonic()
        try:
            response = self.opener.open(request, timeout=120)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            if stream:
                chunks, size = [], 0
                while True:
                    chunk = response.readline(1 << 20)
                    if not chunk:
                        break
                    size += len(chunk)
                    require(size <= 8 << 20, "测试响应超过 8 MiB。")
                    chunks.append(chunk)
                    print(self.clean(chunk.decode(errors="replace")), end="", flush=True)
                raw = b"".join(chunks).decode()
            else:
                data = response.read((8 << 20) + 1)
                require(len(data) <= 8 << 20, "测试响应超过 8 MiB。")
                raw = data.decode()
            status = response.status
            replayed = response.headers.get("Idempotency-Replayed") == "true"
        try:
            result = json.loads(raw, parse_float=Decimal)
        except json.JSONDecodeError:
            result = raw
        self.remember(result)
        record = {"time": datetime.now(timezone.utc).isoformat(), "method": method, "path": path,
                  "status": status, "elapsed_ms": round((time.monotonic() - started) * 1000),
                  "replayed": replayed, "request": body, "response": result}
        folder = self.directory / "results"
        folder.mkdir(exist_ok=True, mode=0o700)
        artifact = folder / (str(time.time_ns()) + ".json")
        write_private(artifact, self.clean(record))
        print(f"HTTP {status} {method} {path} → {artifact.relative_to(self.directory)}" + (" [重放]" if replayed else ""))
        return status, result, raw

    def api(self, method, path, token="", body=None, idem=""):
        status, result, _ = self.request(method, path, token, body, idem)
        require(200 <= status < 300, f"{method} {path} 失败（HTTP {status}），请查看脱敏结果。")
        return result.get("data", result) if isinstance(result, dict) else result

    def login(self, role):
        return self.api("POST", "/api/v1/auth/login", body={
            "email": self.config[role + "_email"], "password": self.config[role + "_password"]})["access_token"]

    def items(self, path, token):
        items, page = [], 1
        while True:
            result = self.api("GET", path + ("&" if "?" in path else "?") + f"page={page}&page_size=100", token)
            items.extend(result["items"])
            if len(items) >= result["total"]:
                return items
            require(result["items"], "分页未完整返回，不能继续对账或创建重复数据。")
            page += 1

    def ensure(self, path, token, body, field="name"):
        query = urllib.parse.urlencode({"search": body[field]})
        found = [v for v in self.items(path + "?" + query, token) if v[field] == body[field]]
        require(len(found) <= 1, f"{path} 存在同名测试资源，请手动检查。")
        return found[0] if found else self.api("POST", path, token, body)

    def environment(self):
        c = self.config
        env = dict(os.environ)
        # Explicit local settings prevent inherited deployment variables affecting this run.
        env.update(DATABASE_URL="postgres://lite_api:" + urllib.parse.quote(c["postgres_password"], safe="")
                   + "@127.0.0.1:15432/lite_api?sslmode=disable", REDIS_URL="redis://127.0.0.1:16379/0",
                   LISTEN_ADDR="127.0.0.1:" + str(urllib.parse.urlsplit(self.base).port), JWT_SECRET=c["jwt_secret"],
                   POSTGRES_PASSWORD=c["postgres_password"], ADMIN_EMAIL=c["admin_email"], ADMIN_PASSWORD=c["admin_password"],
                   PRICING_FILE="", GEMINI_QUOTA_POLICY="", UPSTREAM_PRIVATE_CIDRS="",
                   GATEWAY_CN_PROVIDERS_BALANCE_CHECK_ENABLED="false", GATEWAY_STREAM_DATA_INTERVAL_TIMEOUT="180",
                   GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT="900")
        return env

    def command(self, args):
        result = subprocess.run(args, cwd=ROOT, env=self.environment(), capture_output=True, text=True)
        require(result.returncode == 0, self.clean(result.stderr or result.stdout or "子命令失败"))
        return result.stdout.strip()

    def init(self):
        compose = ["docker", "compose", "--env-file", os.devnull, "-f", str(ROOT / "compose.yaml")]
        print("启动本机 PostgreSQL/Redis 并构建应用；不会清空已有数据库。", flush=True)
        self.command(compose + ["up", "-d", "--wait"])
        version = self.command(["git", "rev-parse", "--short", "HEAD"])
        self.command(["go", "build", "-ldflags", "-X github.com/xrdavies/lite-api/internal/app.Version=" + version,
                      "-o", "bin/lite-api", "./cmd/lite-api"])
        sql = compose + ["exec", "-T", "postgres", "psql", "-U", "lite_api", "-d", "lite_api", "-Atc"]
        count = self.command(sql + ["SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'"])
        if count == "0":
            self.command([str(ROOT / "bin/lite-api"), "init-db"])
        if self.command(sql + ["SELECT count(*) FROM users"]) == "0":
            self.command([str(ROOT / "bin/lite-api"), "bootstrap"])
        print("初始化完成。已有表/用户会保留；请运行 serve，并在另一个终端运行 seed。")

    def seed(self):
        # ponytail: run seed serially; add a file lock only if concurrent operators need it.
        c, prefix = self.config, self.config["fixture_name"]
        require(c["upstream_api_key"], "请先在 config.json 填写 upstream_api_key（或运行 set-key）。")
        admin = self.login("admin")
        user = self.ensure("/api/v1/admin/users", admin, {"email": c["user_email"], "password": c["user_password"],
                           "role": "user", "balance": 0, "restrict_public_groups": True}, "email")
        self.state["user_id"] = user["id"]
        self.save()
        user_token = self.login("user")
        for label, protocol in PROTOCOLS.items():
            name = prefix + "-" + label
            group = self.ensure("/api/v1/admin/groups", admin, {"name": name, "platform": "openai", "is_exclusive": True,
                "rate_multiplier": 1, "long_context_pricing_enabled": False,
                "model_allowlist": {"enabled": True, "models": [c["upstream_model"]]}})
            channel = self.ensure("/api/v1/admin/channels", admin, {"name": name, "group_ids": [group["id"]],
                "billing_model_source": "requested", "model_pricing": [{"platform": "openai", "models": [c["upstream_model"]], **PRICES}]})
            account = self.ensure("/api/v1/admin/accounts", admin, {"name": name, "platform": "openai", "type": "apikey",
                "group_ids": [group["id"]], "credentials": {"api_key": c["upstream_api_key"], "base_url": c["upstream_base_url"],
                "api_protocol": protocol, "model_mapping": {c["upstream_model"]: c["upstream_model"]}}})
            self.state.setdefault(label, {}).update(group_id=group["id"], channel_id=channel["id"], account_id=account["id"])
            self.save()
        if not self.state.get("granted"):
            groups = list(set(user["allowed_groups"]) | {self.state[p]["group_id"] for p in PROTOCOLS})
            self.api("PUT", f"/api/v1/admin/users/{user['id']}", admin, {"allowed_groups": groups})
            self.state["granted"] = True
            self.save()
        if not self.state.get("funded"):
            history = self.items(f"/api/v1/admin/users/{user['id']}/balance-history?type=admin_balance", admin)
            if not any(row.get("notes") == prefix for row in history):
                self.api("POST", f"/api/v1/admin/users/{user['id']}/balance", admin,
                         {"operation": "add", "balance": "10", "notes": prefix}, prefix + "-initial-balance")
            self.state["funded"] = True
            self.save()
        for label in PROTOCOLS:
            key = self.ensure("/api/v1/keys", user_token, {"name": prefix + "-" + label, "group_id": self.state[label]["group_id"], "quota": 10})
            self.state[label].update(key_id=key["id"], key=key["key"])
            self.save()
        print("测试用户、Chat/Responses 两套分组/价卡/账号/Key 已准备。重跑复用现有资源，不覆盖配置或重置余额。")

    def generate(self, args):
        label = args.command
        require(label in self.state, "请先运行 seed。")
        c = self.config
        body = {"model": c["upstream_model"], "stream": args.stream}
        if label == "chat":
            path = "/v1/chat/completions"
            body.update(messages=[{"role": "user", "content": args.prompt}], max_completion_tokens=c["max_output_tokens"])
            if args.stream:
                body["stream_options"] = {"include_usage": True}
        else:
            path = "/v1/responses"
            body.update(input=args.prompt, max_output_tokens=c["max_output_tokens"], store=False)
        status, data, raw = self.request("POST", path, self.state[label]["key"], body, args.idempotency_key, args.stream)
        require(status == 200, "模型请求失败，请查看结果文件；脚本不会自动重试收费请求。")
        if not args.stream:
            print(json.dumps(self.clean(data), ensure_ascii=False, indent=2, default=str))
        generation_ok(raw, label, args.stream)
        print("文本、usage、成功终态检查通过；请人工核对回答，再运行 check 对账。")

    def inspect(self, check=False):
        require("user_id" in self.state and all(p in self.state for p in PROTOCOLS), "请先运行 seed。")
        admin, user = self.login("admin"), self.login("user")
        uid = self.state["user_id"]
        profile = self.api("GET", "/api/v1/user/profile", user)
        usage = self.items(f"/api/v1/admin/usage?user_id={uid}", admin)
        history = self.items(f"/api/v1/admin/users/{uid}/balance-history?type=admin_balance", admin)
        keys = {label: self.api("GET", f"/api/v1/keys/{self.state[label]['key_id']}", user) for label in PROTOCOLS}
        result = {"profile": profile, "keys": keys, "usage": usage, "balance_history": history}
        write_private(self.directory / "inspection.json", self.clean(result))
        if check:
            reconcile(profile, keys, usage, history)
        print(f"用户 {uid}，balance={profile['balance']}，用量 {len(usage)} 条；详情见 inspection.json。")
        if check:
            print("余额及 Key 累计对账通过（单价/倍率仍需人工复核）。" if usage else "零消费对账通过；尚无模型消费，不能视为真实调用通过。")

    def permissions(self):
        token = self.login("user")
        for credential, expected in [(token, 403), (self.state["chat"]["key"], 401), ("", 401)]:
            status, _, _ = self.request("GET", "/api/v1/admin/users", credential)
            require(status == expected, f"权限检查失败：预期 {expected}，实际 {status}。")
        print("普通用户、客户端 Key、匿名访问管理员接口均已拒绝。")

    def plan(self, args):
        admin, item = self.login("admin"), self.state[args.protocol]
        if args.action == "create":
            plans = self.api("GET", f"/api/v1/admin/accounts/{item['account_id']}/scheduled-test-plans", admin)
            matches = [p for p in plans if p["model_id"] == self.config["upstream_model"]]
            require(len(matches) <= 1, "账号存在多个同模型计划，请手动选择。")
            plan = matches[0] if matches else self.api("POST", "/api/v1/admin/scheduled-test-plans", admin, {
                "account_id": item["account_id"], "model_id": self.config["upstream_model"], "cron_expression": "* * * * *",
                "enabled": False, "max_results": 3, "auto_recover": False})
            item["plan_id"] = plan["id"]
            self.save()
            print(f"计划 {plan['id']}，enabled={plan['enabled']}；新建默认停用，enable 后会每分钟真实调用。")
        else:
            path = f"/api/v1/admin/scheduled-test-plans/{item['plan_id']}"
            result = self.api("GET", path + "/results", admin) if args.action == "results" else self.api(
                "PUT", path, admin, {"enabled": args.action == "enable"})
            print(json.dumps(self.clean(result), ensure_ascii=False, indent=2, default=str))


def reconcile(profile, keys, usage, history):
    unit = Decimal("0.00000001")
    debit = lambda rows: sum((Decimal(str(v["actual_cost"])).quantize(unit, rounding=ROUND_HALF_UP) for v in rows), Decimal(0))
    credit = sum((Decimal(str(v["value"])) for v in history), Decimal(0))
    require(credit - debit(usage) == Decimal(str(profile["balance"])), "用户余额与调整账本、消费之和不一致。")
    for label, key in keys.items():
        spent = debit([v for v in usage if v["api_key_id"] == key["id"]])
        require(spent == Decimal(str(key["quota_used"])), f"{label} Key 累计额度与用量不一致（手动 reset_quota 后需要另记基线）。")


def prepare(directory):
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    path = directory / "config.json"
    if not path.exists():
        suffix = secrets.token_hex(4)
        write_private(path, {"base_url": "http://127.0.0.1:8080", "postgres_password": "lite-api-local-only",
            "jwt_secret": secrets.token_hex(32), "admin_email": "admin@lite-api.test", "admin_password": secrets.token_urlsafe(24),
            "fixture_name": "manual-" + suffix, "user_email": "manual-" + suffix + "@example.test",
            "user_password": secrets.token_urlsafe(24), "upstream_base_url": "https://buyonce.xyz",
            "upstream_model": "gpt-5.6-luna", "upstream_api_key": "", "max_output_tokens": 256})
    print(f"本地配置：{path}（已有配置不会覆盖）。密码/密钥仅保存在此文件；填写 upstream_api_key 或运行 set-key。")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dir", type=Path, default=ROOT / "reserve/manual", help="本地配置与结果目录")
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("prepare", "set-key", "init", "serve", "seed", "inspect", "check", "permissions"):
        sub.add_parser(name)
    for name in PROTOCOLS:
        command = sub.add_parser(name, help="发送一次可能计费的真实模型请求")
        command.add_argument("--stream", action="store_true")
        command.add_argument("--prompt", default="Reply only OK.")
        command.add_argument("--idempotency-key", default="")
    health = sub.add_parser("health-test", help="一次可能计费的上游健康测试")
    health.add_argument("protocol", choices=PROTOCOLS)
    plan = sub.add_parser("plan", help="定时健康测试；enable 会持续产生上游请求")
    plan.add_argument("protocol", choices=PROTOCOLS)
    plan.add_argument("action", choices=("create", "enable", "disable", "results"))
    request = sub.add_parser("request", help="使用自动登录的身份发送本地手动请求")
    request.add_argument("method", choices=("GET", "POST", "PUT", "DELETE"))
    request.add_argument("path")
    request.add_argument("--as", dest="role", choices=("admin", "user", "chat", "responses", "anonymous"), default="user")
    request.add_argument("--body", type=Path, help="JSON 请求文件")
    args = parser.parse_args()
    app = None
    try:
        if args.command == "prepare":
            prepare(args.dir)
            return
        app = Manual(args.dir)
        if args.command == "set-key":
            import getpass
            key = getpass.getpass("上游 API Key（不回显）: ").strip()
            require(key and "\n" not in key and "\r" not in key, "API Key 不能为空或包含换行。")
            app.config["upstream_api_key"] = key
            write_private(args.dir / "config.json", app.config)
        elif args.command == "init":
            app.init()
        elif args.command == "serve":
            env = app.environment()
            for key in ("ADMIN_EMAIL", "ADMIN_PASSWORD"):
                env.pop(key, None)
            os.execve(ROOT / "bin/lite-api", ["lite-api", "serve"], env)
        elif args.command == "seed":
            app.seed()
        elif args.command in PROTOCOLS:
            app.generate(args)
        elif args.command in {"inspect", "check"}:
            app.inspect(args.command == "check")
        elif args.command == "permissions":
            app.permissions()
        elif args.command == "health-test":
            token = app.login("admin")
            status, _, raw = app.request("POST", f"/api/v1/admin/accounts/{app.state[args.protocol]['account_id']}/test", token,
                {"model_id": app.config["upstream_model"], "mode": "text", "prompt": "Reply only OK."}, stream=True)
            events = [json.loads(line[5:]) for line in raw.splitlines() if line.startswith("data:")]
            require(status == 200 and any(e.get("type") == "test_complete" and e.get("success") for e in events), "健康测试失败。")
        elif args.command == "plan":
            app.plan(args)
        else:
            token = app.login(args.role) if args.role in {"admin", "user"} else "" if args.role == "anonymous" else app.state[args.role]["key"]
            result = app.api(args.method, args.path, token, load_json(args.body) if args.body else None)
            print(json.dumps(app.clean(result), ensure_ascii=False, indent=2, default=str))
    except (OSError, ValueError, KeyError, RuntimeError, urllib.error.URLError) as e:
        print("失败：" + (app.clean(str(e)) if app else str(e)) + "；首次使用请先 prepare/init/serve/seed。", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("已停止。收费请求结果不明时先查用量，不要直接重复发送。", file=sys.stderr)
        sys.exit(130)


if __name__ == "__main__":
    main()
