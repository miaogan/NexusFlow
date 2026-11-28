"""Seeds a local NexusFlow stack with demo knowledge, users and credentials.

Creates (idempotently):
  * a knowledge base ("示例知识库") and uploads examples/demo/docs/*.md
  * read-only and editor demo users, granted kb:viewer / kb:editor
  * an API key for the OpenAI-compatible endpoint (plaintext printed once)
  * a sample hybrid search and a sample chat turn to prove the stack works

Only the Python standard library is required.

Usage:
    python scripts/seed_demo_data.py
    python scripts/seed_demo_data.py --skip-users --skip-api-key
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DOCS_DIR = ROOT / "examples" / "demo" / "docs"
DEMO_KB_NAME = "示例知识库"
DEMO_PASSWORD_DEFAULT = "Demo@12345"


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
    "NF_GATEWAY", f"http://127.0.0.1:{env_value('GATEWAY_PUBLISH_PORT', '8080')}"
)
ADMIN = env_value("SEED_ADMIN_USERNAME", "admin")
PASSWORD = env_value("SEED_ADMIN_PASSWORD", "admin123")


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
    except Exception as exc:  # noqa: BLE001 - surface network errors to the caller
        return 0, f"{type(exc).__name__}: {exc}"
    try:
        return status, json.loads(body)
    except json.JSONDecodeError:
        return status, body


def fail(message: str) -> None:
    print(f"[ERROR] {message}", file=sys.stderr)
    raise SystemExit(2)


def login() -> str:
    status, body = request(
        "POST", GATEWAY + "/v1/auth/login",
        payload={"username": ADMIN, "password": PASSWORD}, timeout=30,
    )
    token = body.get("access_token", "") if isinstance(body, dict) else ""
    if status != 200 or not token:
        fail(f"admin login failed ({status}): {body}")
    return token


def multipart(filename: str, content: bytes, kb_id: str) -> tuple[bytes, str]:
    boundary = "----nexusflow-demo-" + uuid.uuid4().hex
    body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        "Content-Type: text/markdown\r\n\r\n"
    ).encode() + content + (
        f"\r\n--{boundary}\r\n"
        'Content-Disposition: form-data; name="kb_id"\r\n\r\n'
        f"{kb_id}\r\n--{boundary}--\r\n"
    ).encode()
    return body, f"multipart/form-data; boundary={boundary}"


def ensure_kb(token: str, name: str) -> str:
    status, body = request("GET", GATEWAY + "/v1/knowledge-bases", token=token)
    items = body.get("knowledge_bases", []) if isinstance(body, dict) else []
    for item in items:
        if item.get("name") == name:
            print(f"[kb] reuse existing knowledge base: {name} ({item.get('id')})")
            return str(item.get("id"))
    status, body = request(
        "POST", GATEWAY + "/v1/knowledge-bases", token=token,
        payload={"name": name, "description": "NexusFlow 示例数据（可安全删除）"},
    )
    kb_id = str(((body or {}).get("knowledge_base") or {}).get("id") or "")
    if status != 200 or not kb_id:
        fail(f"create knowledge base failed ({status}): {body}")
    print(f"[kb] created knowledge base: {name} ({kb_id})")
    return kb_id


def list_tasks(token: str, kb_id: str) -> list[dict]:
    status, body = request(
        "GET", GATEWAY + f"/v1/tasks?kb_id={urllib.parse.quote(kb_id)}&limit=500", token=token,
    )
    if status != 200 or not isinstance(body, dict):
        fail(f"list tasks failed ({status}): {body}")
    return body.get("tasks", [])


def upload_docs(token: str, kb_id: str) -> list[str]:
    files = sorted(DOCS_DIR.glob("*.md"))
    if not files:
        fail(f"no sample documents under {DOCS_DIR}")
    existing = {str(task.get("file_name")) for task in list_tasks(token, kb_id)}
    uploaded: list[str] = []
    for path in files:
        if path.name in existing:
            print(f"[docs] skip existing document: {path.name}")
            continue
        raw, ctype = multipart(path.name, path.read_bytes(), kb_id)
        status, body = request(
            "POST", GATEWAY + "/v1/files", token=token, raw=raw, content_type=ctype, timeout=180,
        )
        task_id = str((body or {}).get("task_id") or "") if isinstance(body, dict) else ""
        if status != 202 or not task_id:
            fail(f"upload {path.name} failed ({status}): {body}")
        uploaded.append(task_id)
        print(f"[docs] uploaded {path.name} -> task {task_id}")
    return uploaded


def wait_for_tasks(token: str, kb_id: str, task_ids: list[str]) -> None:
    pending = set(task_ids)
    deadline = time.time() + 240
    while pending and time.time() < deadline:
        for task_id in list(pending):
            status, body = request(
                "GET",
                GATEWAY + f"/v1/tasks/{task_id}?kb_id={urllib.parse.quote(kb_id)}",
                token=token, timeout=30,
            )
            state = str((body or {}).get("status", "")) if isinstance(body, dict) else ""
            if state == "COMPLETED":
                pending.discard(task_id)
            elif state == "FAILED":
                error = (body or {}).get("error_msg", "")
                fail(f"task {task_id} failed: {error}")
        if pending:
            time.sleep(3)
    if pending:
        fail(f"tasks did not finish in time: {sorted(pending)}")
    if task_ids:
        print(f"[docs] parsed and indexed {len(task_ids)} document(s)")


def ensure_user(token: str, username: str, password: str, roles: list[str]) -> str:
    status, body = request(
        "GET", GATEWAY + f"/v1/users?query={urllib.parse.quote(username)}&limit=50", token=token,
    )
    users = (body.get("users") or []) if isinstance(body, dict) else []
    for user in users:
        if user.get("username") == username:
            print(f"[users] reuse existing user: {username}")
            return str(user.get("id"))
    status, body = request(
        "POST", GATEWAY + "/v1/users", token=token,
        payload={"username": username, "password": password, "roles": roles},
    )
    user = (body or {}).get("user") or {}
    user_id = str(user.get("id") or "")
    if status not in (200, 201) or not user_id:
        fail(f"create user {username} failed ({status}): {body}")
    print(f"[users] created {username} with roles {roles}")
    return user_id


def grant_kb(token: str, kb_id: str, user_id: str, permission: str) -> None:
    status, body = request(
        "POST", GATEWAY + f"/v1/knowledge-bases/{kb_id}/permissions", token=token,
        payload={"subject_type": "user", "subject_id": user_id, "permission": permission},
    )
    if status != 200:
        fail(f"grant {permission} failed ({status}): {body}")
    print(f"[users] granted {permission} on the demo KB")


def ensure_api_key(token: str, kb_id: str) -> str:
    status, body = request("GET", GATEWAY + "/v1/api-keys", token=token)
    keys = body.get("api_keys", []) if isinstance(body, dict) else []
    if any(key.get("name") == "demo-openai" for key in keys):
        print("[api] demo-openai already exists; plaintext cannot be shown again")
        return ""
    status, body = request(
        "POST", GATEWAY + "/v1/api-keys", token=token,
        payload={
            "name": "demo-openai",
            "scopes": ["chat:completions", "search:use"],
            "allowed_kb_ids": [kb_id],
            "rate_limit_rpm": 60,
        },
    )
    plain = (body or {}).get("key", "") if isinstance(body, dict) else ""
    if status != 201 or not plain:
        fail(f"create api key failed ({status}): {body}")
    print("[api] created API key demo-openai (shown once below)")
    return plain


def sample_search(token: str, kb_id: str) -> int:
    status, body = request(
        "POST", GATEWAY + "/v1/search", token=token,
        payload={
            "query": "NexusFlow 如何融合多路召回？",
            "routes": [
                {"type": "vector", "top_k": 10},
                {"type": "bm25", "top_k": 10},
                {"type": "graph", "top_k": 10},
            ],
            "final_top_n": 5,
            "kb_id": kb_id,
        },
    )
    chunks = body.get("chunks", []) if isinstance(body, dict) else []
    if status != 200:
        fail(f"sample search failed ({status}): {body}")
    print(f"[check] hybrid search returned {len(chunks)} chunk(s)")
    for chunk in chunks[:2]:
        preview = str(chunk.get("content", "")).replace("\n", " ")[:80]
        print(f"        - {preview}")
    return len(chunks)


def sample_chat(token: str, kb_id: str) -> None:
    status, body = request(
        "POST", GATEWAY + "/v1/chat", token=token,
        payload={"message": "NexusFlow 的混合检索包含哪些召回路由？", "kb_id": kb_id},
        timeout=180,
    )
    if status != 200 or not isinstance(body, dict):
        print(f"[check] chat skipped (LLM not configured or unavailable, status={status})")
        return
    answer = str(body.get("answer", "")).replace("\n", " ")
    citations = body.get("citations", [])
    print(f"[check] chat answered with {len(citations)} citation(s): {answer[:120]}...")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kb-name", default=DEMO_KB_NAME)
    parser.add_argument("--password", default=os.environ.get("NF_DEMO_PASSWORD", DEMO_PASSWORD_DEFAULT))
    parser.add_argument("--skip-users", action="store_true")
    parser.add_argument("--skip-api-key", action="store_true")
    parser.add_argument("--skip-check", action="store_true")
    args = parser.parse_args()

    print(f"gateway: {GATEWAY}")
    token = login()
    kb_id = ensure_kb(token, args.kb_name)
    task_ids = upload_docs(token, kb_id)
    wait_for_tasks(token, kb_id, task_ids)

    if not args.skip_users:
        viewer_id = ensure_user(token, "demo-viewer", args.password, ["viewer"])
        editor_id = ensure_user(token, "demo-editor", args.password, ["editor"])
        grant_kb(token, kb_id, viewer_id, "kb:viewer")
        grant_kb(token, kb_id, editor_id, "kb:editor")

    api_key = "" if args.skip_api_key else ensure_api_key(token, kb_id)

    if not args.skip_check:
        sample_search(token, kb_id)
        sample_chat(token, kb_id)

    print("\n== demo environment ==")
    print(f"console : http://127.0.0.1:{env_value('WEB_PUBLISH_PORT', '8088')}")
    print(f"gateway : {GATEWAY}")
    print(f"kb      : {args.kb_name} ({kb_id})")
    if not args.skip_users:
        print(f"accounts: admin/{PASSWORD} (admin), demo-editor/{args.password} (editor), demo-viewer/{args.password} (viewer)")
    if api_key:
        print(f"api key : {api_key}  (store it now; it is not shown again)")
    print("\nNext steps:")
    print(f"  curl -s {GATEWAY}/v1/search -H 'Content-Type: application/json' \\")
    print(f"    -d '{{\"query\":\"混合检索\",\"kb_id\":\"{kb_id}\"}}'")
    print("  python scripts/smoke_test.py            # release smoke checks")
    print("  docs/user-guide.md                      # console and API walkthrough")
    return 0


if __name__ == "__main__":
    sys.exit(main())
