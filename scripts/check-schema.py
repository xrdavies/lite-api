#!/usr/bin/env python3
"""Restore the SQL snapshot in an isolated PostgreSQL container and check its contract."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid

root = Path(__file__).resolve().parent.parent
if sys.argv[1:] not in ([], ["--write-contract"]):
    raise SystemExit("Usage: check-schema.py [--write-contract]")
docker = ["docker"]
if os.getenv("DOCKER_CONTEXT"):
    docker += ["--context", os.environ["DOCKER_CONTEXT"]]
container = "lite-api-contract-" + uuid.uuid4().hex[:12]


def run(args, **kwargs):
    result = subprocess.run(docker + args, capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError(result.stderr.decode())
    return result.stdout.decode()


def sql(content):
    return run(["exec", "-i", container, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-At", "-d", "postgres"], input=content.encode())


try:
    run(["run", "-d", "--name", container, "--network", "none", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"])
    for attempt in range(60):
        ready = subprocess.run(docker + ["exec", container, "pg_isready", "-U", "postgres"], capture_output=True)
        if ready.returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("PostgreSQL did not become ready")
    sql((root / "schema/baseline.sql").read_text())
    actual = json.loads(sql((root / "schema/contract.sql").read_text()))
    contract = root / "schema/contract.json"
    if "--write-contract" in sys.argv:
        contract.write_text("[\n" + "\n".join(json.dumps(item, ensure_ascii=False, sort_keys=True) + ("," if i < len(actual) - 1 else "") for i, item in enumerate(actual)) + "\n]\n")
    else:
        assert actual == json.loads(contract.read_text()), "Database contract differs from SQL snapshot"
    assert sql("SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'").strip() == "97"
    print("PASS: SQL restores into an empty database; 97 tables; schema contract matches.")
finally:
    subprocess.run(docker + ["rm", "--force", container], capture_output=True)
