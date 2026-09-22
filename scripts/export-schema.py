#!/usr/bin/env python3
"""Build the pinned schema without plugin tables or business seeds.

Requires the reference checkout only to regenerate the snapshot. Normal use of
lite-api depends solely on the checked-in SQL.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import uuid

root = Path(__file__).resolve().parent.parent
if len(sys.argv) != 2:
    raise SystemExit("Usage: export-schema.py PATH_TO_PINNED_MIGRATIONS")
source = Path(sys.argv[1])
manifest = json.loads((root / "schema/source.json").read_text())
files = sorted(source.glob("*.sql"))
actual = {p.name: hashlib.sha256(p.read_text().strip().encode()).hexdigest() for p in files}
if actual != manifest["migrations"]:
    raise SystemExit("Reference migrations differ from the pinned manifest")

docker = ["docker"]
if os.getenv("DOCKER_CONTEXT"):
    docker += ["--context", os.environ["DOCKER_CONTEXT"]]
container = "lite-api-schema-" + uuid.uuid4().hex[:12]


def run(args, **kwargs):
    return subprocess.run(docker + args, check=True, capture_output=True, **kwargs)


def sql(content):
    try:
        return run(["exec", "-i", container, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres"], input=content.encode())
    except subprocess.CalledProcessError as exc:
        print(exc.stderr.decode(), file=sys.stderr)
        raise


try:
    run(["run", "--detach", "--name", container, "--network", "none", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"])
    for attempt in range(60):
        ready = subprocess.run(docker + ["exec", container, "pg_isready", "-U", "postgres"], capture_output=True)
        if ready.returncode == 0:
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("PostgreSQL did not become ready")
    sql("CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW());")
    for index, p in enumerate(files):
        # psql splits SQL correctly, including dollar-quoted functions and notx
        # concurrent indexes. This database has no application or user traffic.
        content = p.read_text()
        if not p.name.endswith("_notx.sql"):
            content = "BEGIN;\n" + content + "\nCOMMIT;\n"
        sql(content)
        if index % 40 == 0:
            print(f"Applied {index + 1}/{len(files)}: {p.name}", flush=True)
    # The approved schema exception applies only to this disposable empty DB.
    # No CASCADE: unexpected dependencies must fail instead of changing other tables.
    sql("DROP TABLE public.sub2api_plugin_bindings, public.sub2api_plugin_installations;")
    dump = run(["exec", container, "pg_dump", "-U", "postgres", "--schema-only", "--no-owner", "--no-privileges", "postgres"]).stdout.decode()
    # pg_dump adds random restriction keys and executable psql directives; the
    # snapshot is executed through database/sql, and contains SQL statements only.
    dump = "\n".join(line for line in dump.splitlines() if not line.startswith("\\")).rstrip() + "\n"
    if "sub2api_plugin_" in dump:
        raise RuntimeError("Plugin objects remain in the exported schema")
    (root / "schema/baseline.sql").write_text(dump)
    print(f"Exported schema from {len(files)} unchanged migrations; two plugin tables omitted, no row data exported.")
finally:
    subprocess.run(docker + ["rm", "--force", container], capture_output=True)
