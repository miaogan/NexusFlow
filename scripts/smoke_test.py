"""Release smoke checks for a running NexusFlow Compose stack.

Uses only the Python standard library and the Docker CLI. Run after `make up`.
The default mode permits the built-in mock parser but reports real MinerU as
SKIP. Use --require-mineru as a release gate for the external parser.
"""
from __future__ import annotations

import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose"]
if (ROOT / ".env").is_file():
    COMPOSE += ["--env-file", ".env"]
COMPOSE += ["-f", "deploy/docker-compose.yml"]

RESULTS: list[tuple[str, str, str]] = []


def env_value(key: str, default: str = "") -> str:
    if key in os.environ:
        return os.environ[key]
    path = ROOT / ".env"
    if path.is_file():
        for raw in path.read_text(encoding="utf-8").splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            name, value = line.split("=", 1)
            if name.strip() == key:
                value = value.strip()
                if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "'"}:
                    value = value[1:-1]
                return value
    return default


GATEWAY = os.environ.get(
    "SMOKE_GATEWAY_URL",
    f"http://127.0.0.1:{env_value('GATEWAY_PUBLISH_PORT', '8080')}",
)
LLAMAINDEX = os.environ.get("SMOKE_LLAMAINDEX_URL", "http://127.0.0.1:8000")
QDRANT = os.environ.get("SMOKE_QDRANT_URL", "http://127.0.0.1:6333")
AGENT_HUB = os.environ.get("SMOKE_AGENT_HUB_URL", "http://127.0.0.1:8001")
TENANT = os.environ.get("SMOKE_TENANT_ID", env_value("SEED_TENANT_ID", "default"))
ADMIN = os.environ.get("SMOKE_ADMIN_USERNAME", env_value("SEED_ADMIN_USERNAME", "admin"))
PASSWORD = os.environ.get("SMOKE_ADMIN_PASSWORD", env_value("SEED_ADMIN_PASSWORD", "admin123"))


def result(status: str, name: str, detail: str = "") -> None:
    RESULTS.append((status, name, detail))
    suffix = f" | {detail}" if detail else ""
    print(f"{status} {name}{suffix}", flush=True)


def check(name: str, ok: bool, detail: str = "") -> None:
    result("PASS" if ok else "FAIL", name, detail)


def skip(name: str, detail: str) -> None:
    result("SKIP", name, detail)


def compose(
    args: list[str], timeout: float = 30, allow_failure: bool = False, stdin: str | None = None,
) -> subprocess.CompletedProcess[str]:
    proc = subprocess.run(
        COMPOSE + args,
        cwd=ROOT,
        input=stdin,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=timeout,
        check=False,
    )
    if proc.returncode and not allow_failure:
        detail = (proc.stderr or proc.stdout).strip()
        raise RuntimeError(f"docker compose {' '.join(args)}: {detail}")
    return proc


def container_env(service: str, key: str) -> str:
    proc = compose(["exec", "-T", service, "printenv", key], allow_failure=True)
    if proc.returncode not in (0, 1):
        raise RuntimeError(f"read {service}:{key}: {(proc.stderr or proc.stdout).strip()}")
    return proc.stdout.strip()


def request(
    method: str,
    url: str,
    token: str | None = None,
    payload: dict | list | None = None,
    raw: bytes | None = None,
    content_type: str | None = None,
    timeout: float = 120,
) -> tuple[int, object]:
    data = raw
    headers: dict[str, str] = {}
    if payload is not None:
        data = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    if content_type:
        headers["Content-Type"] = content_type
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode(errors="replace")
            status = resp.status
    except urllib.error.HTTPError as exc:
        body = exc.read().decode(errors="replace")
        status = exc.code
    except Exception as exc:  # Network failures are reported as check failures.
        return 0, f"{type(exc).__name__}: {exc}"
    try:
        return status, json.loads(body)
    except json.JSONDecodeError:
        return status, body


def login() -> tuple[str, int, object]:
    status, body = request(
        "POST",
        GATEWAY + "/v1/auth/login",
        payload={"username": ADMIN, "password": PASSWORD},
        timeout=30,
    )
    token = body.get("access_token", "") if isinstance(body, dict) else ""
    return token, status, body


def multipart(field: str, filename: str, content: bytes, mime: str) -> tuple[bytes, str]:
    boundary = "----nexusflow-smoke-" + uuid.uuid4().hex
    body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="{field}"; filename="{filename}"\r\n'
        f"Content-Type: {mime}\r\n\r\n"
    ).encode() + content + f"\r\n--{boundary}--\r\n".encode()
    return body, f"multipart/form-data; boundary={boundary}"


def minimal_pdf(marker: str) -> bytes:
    """Create a one-page text PDF without requiring third-party packages."""
    stream = (
        f"BT /F1 12 Tf 72 720 Td (NexusFlow smoke {marker}) Tj "
        "0 -24 Td (MinerU parses this PDF into Markdown for Qdrant retrieval.) Tj ET"
    ).encode()
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
    ]
    document = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = [0]
    for number, obj in enumerate(objects, 1):
        offsets.append(len(document))
        document.extend(f"{number} 0 obj\n".encode() + obj + b"\nendobj\n")
    xref = len(document)
    document.extend(f"xref\n0 {len(objects) + 1}\n".encode())
    document.extend(b"0000000000 65535 f \n")
    for offset in offsets[1:]:
        document.extend(f"{offset:010d} 00000 n \n".encode())
    document.extend(
        f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode()
    )
    return bytes(document)


def token_id(token: str) -> str:
    parts = token.split(".")
    if len(parts) != 3:
        return ""
    encoded = parts[1] + "=" * (-len(parts[1]) % 4)
    payload = json.loads(base64.urlsafe_b64decode(encoded))
    return str(payload.get("jti", ""))


def health_checks() -> None:
    required = {
        "auth-service", "gateway", "ingestion", "llamaindex", "postgres",
        "processing-worker", "qdrant", "query-service", "rabbitmq", "redis", "user-service",
    }
    running = set(compose(["ps", "--status", "running", "--services"]).stdout.splitlines())
    missing = sorted(required - running)
    check("Compose middleware and core services running", not missing, f"missing={missing}")
    for name, url in (
        ("gateway", GATEWAY + "/healthz"),
        ("llamaindex", LLAMAINDEX + "/healthz"),
        ("qdrant", QDRANT + "/healthz"),
    ):
        status, _ = request("GET", url, timeout=10)
        check(f"{name} /healthz", status == 200, f"status={status}")


def postgres_checks() -> None:
    for service in ("auth-service", "user-service"):
        driver = container_env(service, "IDENTITY_DRIVER")
        check(f"{service} uses Postgres identity", driver == "postgres", f"IDENTITY_DRIVER={driver!r}")

    query = """
SELECT json_build_object(
  'users_table', to_regclass('public.identity_users') IS NOT NULL,
  'roles_table', to_regclass('public.identity_roles') IS NOT NULL,
  'admin_count', (SELECT count(*) FROM identity_users
                  WHERE tenant_id = :'tenant' AND lower(username) = lower(:'admin')
                    AND status = 'active' AND roles ? 'admin'),
  'builtin_roles', (SELECT count(*) FROM identity_roles WHERE builtin = TRUE)
);
"""
    output = compose([
        "exec", "-T", "postgres", "psql", "-U", "nexusflow", "-d", "nexusflow", "-At",
        "-v", f"tenant={TENANT}", "-v", f"admin={ADMIN}",
    ], stdin=query).stdout.strip()
    data = json.loads(output)
    check(
        "Postgres identity tables and seed admin",
        data.get("users_table") is True
        and data.get("roles_table") is True
        and data.get("admin_count") == 1
        and data.get("builtin_roles", 0) >= 1,
        f"tables={data.get('users_table')}/{data.get('roles_table')} "
        f"admin_count={data.get('admin_count')} builtin_roles={data.get('builtin_roles')}",
    )

    token, status, _ = login()
    check("Seed admin login through auth-service", status == 200 and bool(token), f"status={status}")
    if token:
        status, me = request("GET", GATEWAY + "/v1/me", token=token, timeout=20)
        roles = me.get("roles", []) if isinstance(me, dict) else []
        check(
            "Gateway reads seeded identity",
            status == 200 and isinstance(me, dict) and me.get("username") == ADMIN and "admin" in roles,
            f"status={status} roles={roles}",
        )


def redis_checks() -> None:
    for service in ("auth-service", "gateway"):
        driver = container_env(service, "TOKEN_BLACKLIST_DRIVER")
        check(f"{service} uses Redis revocation", driver == "redis", f"TOKEN_BLACKLIST_DRIVER={driver!r}")

    token, status, _ = login()
    check("Login for Redis revocation test", status == 200 and bool(token), f"status={status}")
    if not token:
        return
    jti = token_id(token)
    check("Access token contains jti", bool(jti))
    if not jti:
        return

    status, _ = request("POST", GATEWAY + "/v1/auth/logout", token=token, payload={}, timeout=20)
    check("Logout succeeds", status == 200, f"status={status}")
    key = "auth:revoked:" + jti
    exists = compose(["exec", "-T", "redis", "redis-cli", "EXISTS", key]).stdout.strip()
    ttl = compose(["exec", "-T", "redis", "redis-cli", "TTL", key]).stdout.strip()
    check("Redis stores revoked jti with TTL", exists == "1" and ttl.isdigit() and int(ttl) > 0, f"exists={exists} ttl={ttl}")

    status, _ = request("GET", GATEWAY + "/v1/me", token=token, timeout=20)
    check("Gateway rejects revoked token across processes", status == 401, f"status={status}")
    new_token, status, _ = login()
    check("Fresh login remains usable after revocation", status == 200 and bool(new_token), f"status={status}")


def object_store_precheck(real_mineru: bool) -> None:
    if not real_mineru:
        return
    base = container_env("processing-worker", "OBJECT_STORE_BASE")
    directory = container_env("processing-worker", "OBJECT_STORE_DIR")
    parsed = urllib.parse.urlparse(base)
    check(
        "MinerU reads the shared local object volume",
        parsed.scheme in {"http", "https"} and bool(parsed.hostname) and bool(directory),
        f"base={base} directory={directory}",
    )


def qdrant_checks(require_mineru: bool) -> None:
    llamaindex_url = container_env("processing-worker", "LLAMAINDEX_URL")
    mineru_target = container_env("processing-worker", "MINERU_TARGET")
    real_mineru = bool(mineru_target)
    check("Processing worker indexes through LlamaIndex", bool(llamaindex_url), f"configured={bool(llamaindex_url)}")
    if real_mineru:
        check("Real MinerU endpoint configured", True)
    elif require_mineru:
        check("Real MinerU endpoint configured", False, "MINERU_TARGET is empty; mock parsing is not a release substitute")
    else:
        skip("Real MinerU parse -> Qdrant retrieval", "MINERU_TARGET is empty; validating mock parser -> Qdrant separately")
    object_store_precheck(real_mineru)

    token, status, _ = login()
    check("Login for indexing smoke test", status == 200 and bool(token), f"status={status}")
    if not token:
        return

    marker = "nf-smoke-" + uuid.uuid4().hex
    markdown = (
        f"# NexusFlow smoke {marker}\n\n"
        f"真实解析联调标记为 {marker}。NexusFlow 将文件解析为 Markdown，"
        "随后完成切块、向量嵌入、Qdrant 入库与检索。\n"
    )
    filename = f"{marker}.md"
    document = markdown.encode()
    mime = "text/markdown"
    if real_mineru:
        filename = f"{marker}.pdf"
        document = minimal_pdf(marker)
        mime = "application/pdf"
    raw, content_type = multipart("file", filename, document, mime)
    status, body = request(
        "POST", GATEWAY + "/v1/files", token=token, raw=raw, content_type=content_type, timeout=60,
    )
    task_id = body.get("task_id", "") if isinstance(body, dict) else ""
    check("Upload smoke document", status == 202 and bool(task_id), f"status={status} task_id={task_id}")
    if not task_id:
        return

    final_status = ""
    task: object = {}
    deadline = time.time() + 240
    while time.time() < deadline:
        status, task = request("GET", f"{GATEWAY}/v1/tasks/{task_id}", token=token, timeout=20)
        final_status = str(task.get("status", "")) if isinstance(task, dict) else ""
        if final_status in {"COMPLETED", "FAILED"}:
            break
        time.sleep(3)
    error = task.get("error_msg", "") if isinstance(task, dict) else task
    check(
        f"Upload -> {'MinerU' if real_mineru else 'mock parser'} -> indexing completes",
        final_status == "COMPLETED",
        f"status={final_status} error={error}",
    )
    if final_status != "COMPLETED":
        return

    scroll = {
        "filter": {"must": [{"key": "task_id", "match": {"value": task_id}}]},
        "limit": 20,
        "with_payload": True,
        "with_vector": True,
    }
    status, body = request(
        "POST", QDRANT + "/collections/nexusflow_default/points/scroll", payload=scroll, timeout=30,
    )
    points = body.get("result", {}).get("points", []) if isinstance(body, dict) else []
    check("Qdrant contains chunks for this task", status == 200 and bool(points), f"status={status} points={len(points)}")

    def vector_size(point: dict) -> int:
        vector = point.get("vector")
        if isinstance(vector, list):
            return len(vector)
        if isinstance(vector, dict):
            return sum(len(item) for item in vector.values() if isinstance(item, list))
        return 0

    dimensions = [vector_size(point) for point in points]
    check("Qdrant points contain real embedding vectors", bool(points) and all(size > 0 for size in dimensions), f"dimensions={dimensions[:3]}")
    tenants = [point.get("payload", {}).get("tenant_id") for point in points]
    check("Qdrant chunks carry the correct tenant", bool(points) and all(value == TENANT for value in tenants), f"tenants={sorted(set(map(str, tenants)))}")

    query = marker if real_mineru else "NexusFlow 查询链路"
    status, body = request(
        "POST", LLAMAINDEX + "/v1/search", payload={
            "query": query,
            "routes": [{"type": "vector", "top_k": 5}],
            "final_top_n": 5,
            "kb_id": "default",
            "filters": {"tenant_id": TENANT, "task_id": task_id},
        }, timeout=120,
    )
    chunks = body.get("chunks", []) if isinstance(body, dict) else []
    check("LlamaIndex retrieves this task from Qdrant", status == 200 and bool(chunks), f"status={status} chunks={len(chunks)}")
    if real_mineru:
        # PDF layout extraction may insert line breaks inside long identifiers.
        found = any(marker in "".join(str(chunk.get("content", "")).split()) for chunk in chunks)
        check("Real MinerU output preserves the uploaded marker (ignoring layout whitespace)", found, f"marker={marker}")
    else:
        found = any("NexusFlow" in str(chunk.get("content", "")) for chunk in chunks)
        check("Mock parser path is explicitly identified", found)

    status, body = request(
        "POST", GATEWAY + "/v1/search", token=token, payload={
            "query": query,
            "routes": [
                {"type": "vector", "top_k": 10},
                {"type": "bm25", "top_k": 10},
                {"type": "graph", "top_k": 10},
            ],
            "final_top_n": 5,
        }, timeout=120,
    )
    gateway_chunks = body.get("chunks", []) if isinstance(body, dict) else []
    check("Gateway hybrid search reads the real retrieval service", status == 200 and bool(gateway_chunks), f"status={status} chunks={len(gateway_chunks)}")


def agent_hub_checks() -> None:
    """Read-only checks for the custom SubAgent registry and skill routing."""
    status, _ = request("GET", AGENT_HUB + "/readyz", timeout=15)
    check("Agent hub readyz", status == 200, f"status={status}")

    status, body = request("GET", AGENT_HUB + "/v1/agents", timeout=15)
    agents = body.get("agents", []) if isinstance(body, dict) else []
    check(
        "Agent hub registry endpoint",
        status == 200 and isinstance(agents, list),
        f"status={status} agents={len(agents)}",
    )

    status, body = request("GET", AGENT_HUB + "/v1/skills", timeout=15)
    skills = body.get("skills", []) if isinstance(body, dict) else []
    check(
        "Agent hub skills endpoint",
        status == 200 and isinstance(skills, list),
        f"status={status} skills={len(skills)}",
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-mineru", action="store_true", help="fail instead of skipping when MINERU_TARGET is empty")
    args = parser.parse_args()

    sections = (
        ("Compose health", health_checks),
        ("Agent hub registry", agent_hub_checks),
        ("Postgres identity", postgres_checks),
        ("Redis revocation", redis_checks),
        ("Parsing and Qdrant", lambda: qdrant_checks(args.require_mineru)),
    )
    for name, section in sections:
        print(f"\n== {name} ==", flush=True)
        try:
            section()
        except Exception as exc:  # Keep later independent diagnostics visible.
            check(f"{name} section executes", False, f"{type(exc).__name__}: {exc}")

    passed = sum(status == "PASS" for status, _, _ in RESULTS)
    failed = [name for status, name, _ in RESULTS if status == "FAIL"]
    skipped = sum(status == "SKIP" for status, _, _ in RESULTS)
    print(f"\n{passed} passed, {len(failed)} failed, {skipped} skipped")
    if failed:
        print("Failed checks: " + ", ".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
