#!/usr/bin/env python3
"""Run Go integration and race checks using disposable local databases."""
import os
from pathlib import Path
import subprocess
import time
import uuid

root = Path(__file__).resolve().parent.parent
docker = ["docker"]
if os.getenv("DOCKER_CONTEXT"):
    docker += ["--context", os.environ["DOCKER_CONTEXT"]]
suffix = uuid.uuid4().hex[:12]
postgres, redis = "lite-api-test-pg-" + suffix, "lite-api-test-redis-" + suffix


def run(args):
    result = subprocess.run(docker + args, capture_output=True, text=True, check=True)
    return result.stdout.strip()


try:
    run(["run", "-d", "--name", postgres, "-p", "127.0.0.1::5432", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"])
    run(["run", "-d", "--name", redis, "-p", "127.0.0.1::6379", "redis:7-alpine@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499"])
    for attempt in range(60):
        ready = subprocess.run(docker + ["exec", postgres, "pg_isready", "-U", "postgres"], capture_output=True)
        if ready.returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("PostgreSQL did not become ready")
    env = os.environ.copy()
    env["TEST_DATABASE_URL"] = "postgres://postgres@" + run(["port", postgres, "5432/tcp"]) + "/postgres?sslmode=disable"
    env["TEST_REDIS_URL"] = "redis://" + run(["port", redis, "6379/tcp"])
    subprocess.run(["go", "test", "-race", "-count=1", "./..."], cwd=root, env=env, check=True)
finally:
    subprocess.run(docker + ["rm", "--force", postgres, redis], capture_output=True)
