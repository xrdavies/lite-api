#!/usr/bin/env python3
"""Run Go integration and race checks using disposable local databases."""
import os
from pathlib import Path
import secrets
import subprocess
import time
import urllib.error
import urllib.request
import uuid

root = Path(__file__).resolve().parent.parent
docker = ["docker"]
if os.getenv("DOCKER_CONTEXT"):
    docker += ["--context", os.environ["DOCKER_CONTEXT"]]
suffix = uuid.uuid4().hex[:12]
postgres, redis = "lite-api-test-pg-" + suffix, "lite-api-test-redis-" + suffix
storage = "lite-api-test-s3-" + suffix


def run(args, **kwargs):
    result = subprocess.run(docker + args, capture_output=True, text=True, check=True, **kwargs)
    return result.stdout.strip()


try:
    run(["run", "-d", "--name", postgres, "-p", "127.0.0.1::5432", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"])
    run(["run", "-d", "--name", redis, "-p", "127.0.0.1::6379", "redis:7-alpine@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"])
    storage_env = dict(os.environ, MINIO_ROOT_USER=secrets.token_hex(12), MINIO_ROOT_PASSWORD=secrets.token_hex(24))
    # A pinned S3 protocol test server, not an application deployment dependency.
    run(["run", "-d", "--name", storage, "-p", "127.0.0.1::9000", "-e", "MINIO_ROOT_USER", "-e", "MINIO_ROOT_PASSWORD",
         "quay.io/minio/minio@sha256:a1ea29fa28355559ef137d71fc570e508a214ec84ff8083e39bc5428980b015e", "server", "/data"], env=storage_env)
    for attempt in range(60):
        # The entrypoint's temporary initialization server only accepts sockets.
        ready = subprocess.run(docker + ["exec", postgres, "pg_isready", "-h", "127.0.0.1", "-U", "postgres"], capture_output=True)
        if ready.returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("PostgreSQL did not become ready")
    env = os.environ.copy()
    env["TEST_DATABASE_URL"] = "postgres://postgres@" + run(["port", postgres, "5432/tcp"]) + "/postgres?sslmode=disable"
    env["TEST_REDIS_URL"] = "redis://" + run(["port", redis, "6379/tcp"])
    env["TEST_S3_ENDPOINT"] = "http://" + run(["port", storage, "9000/tcp"])
    env["TEST_S3_ACCESS_KEY"] = storage_env["MINIO_ROOT_USER"]
    env["TEST_S3_SECRET_KEY"] = storage_env["MINIO_ROOT_PASSWORD"]
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for attempt in range(60):
        try:
            with client.open(env["TEST_S3_ENDPOINT"] + "/minio/health/ready", timeout=1) as response:
                if response.status == 200:
                    break
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.5)
    else:
        raise RuntimeError("S3 test storage did not become ready")
    command = ["go", "test", "-race", "-count=1"]
    if env.get("TEST_UPSTREAM_API_KEY"):
        command.append("-v")
    subprocess.run(command + ["./..."], cwd=root, env=env, check=True)
finally:
    subprocess.run(docker + ["rm", "--force", "--volumes", postgres, redis, storage], capture_output=True)
