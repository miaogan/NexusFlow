"""Seeds a local NexusFlow stack with demo knowledge, users and platform data.

Creates (idempotently):
  * a knowledge base ("示例知识库") and uploads examples/demo/docs/*.md
  * read-only and editor demo users, granted kb:viewer / kb:editor
  * an API key for the OpenAI-compatible endpoint (plaintext printed once)
  * a sample hybrid search and a sample chat turn to prove the stack works
  * a default-KB document, a reference KB, workflow definitions/versions/runs,
    a sandbox SubAgent, webhooks + a test delivery, extra API keys, and demo
    approvals/audit events (disable with --skip-platform)

Only the Python standard library is required. The approvals/audit step uses
`docker exec` against the compose Postgres container when available.

Usage:
    python scripts/seed_demo_data.py
    python scripts/seed_demo_data.py --skip-users --skip-api-key
    python scripts/seed_demo_data.py --skip-platform
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
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
HUB = os.environ.get(
    "NF_AGENT_HUB", f"http://127.0.0.1:{env_value('AGENT_HUB_PUBLISH_PORT', '8001')}"
)
HUB_TOKEN = env_value("AGENT_HUB_ADMIN_TOKEN", "nexusflow-agent-admin")
PG_CONTAINER = os.environ.get("NF_PG_CONTAINER", "nexusflow-postgres-1")
PG_USER = env_value("POSTGRES_USER", "nexusflow")
PG_DB = env_value("POSTGRES_DB", "nexusflow")


def request(
    method: str,
    url: str,
    token: str | None = None,
    payload: dict | list | None = None,
    raw: bytes | None = None,
    content_type: str | None = None,
    timeout: float = 120,
    extra_headers: dict[str, str] | None = None,
) -> tuple[int, object]:
    data = raw
    headers: dict[str, str] = dict(extra_headers or {})
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


def me(token: str) -> str:
    status, body = request("GET", GATEWAY + "/v1/me", token=token)
    if status != 200 or not isinstance(body, dict):
        return ""
    return str(body.get("user_id") or "")


def ensure_default_doc(token: str) -> str:
    """Puts one document in the default KB so a fresh console has data."""
    name = "01-product-overview.md"
    if any(str(task.get("file_name")) == name for task in list_tasks(token, "default")):
        print("[platform] default KB already has the overview document")
        return "default"
    path = DOCS_DIR / name
    if not path.is_file():
        print(f"[platform] skip default document: {path} not found")
        return "default"
    raw, ctype = multipart(name, path.read_bytes(), "default")
    status, body = request(
        "POST", GATEWAY + "/v1/files", token=token, raw=raw, content_type=ctype, timeout=180,
    )
    task_id = str((body or {}).get("task_id") or "") if isinstance(body, dict) else ""
    if status != 202 or not task_id:
        print(f"[platform] default document upload skipped ({status}): {body}")
        return "default"
    print(f"[platform] uploaded {name} to the default KB -> task {task_id}")
    wait_for_tasks(token, "default", [task_id])
    return "default"


def ensure_reference_kb(token: str, source_id: str) -> str:
    status, body = request("GET", GATEWAY + "/v1/knowledge-bases", token=token)
    items = (body.get("knowledge_bases") or []) if isinstance(body, dict) else []
    for item in items:
        if item.get("name") == "示例知识库（引用）":
            print("[platform] reference KB already exists")
            return str(item.get("id"))
    status, body = request(
        "POST", GATEWAY + "/v1/knowledge-bases", token=token,
        payload={
            "name": "示例知识库（引用）",
            "description": "引用型示例：复用源库向量，不重复上传与 embedding",
            "source_kb_id": source_id,
        },
    )
    kb_id = str(((body or {}).get("knowledge_base") or {}).get("id") or "")
    if status != 200 or not kb_id:
        print(f"[platform] reference KB skipped ({status}): {body}")
        return ""
    print(f"[platform] created reference KB {kb_id} -> source {source_id}")
    return kb_id


def node(node_id: str, node_type: str, config: dict | None = None) -> dict:
    out: dict = {"id": node_id, "type": node_type}
    if config is not None:
        out["config"] = config
    return out


def dag(name: str, nodes: list[dict], edges: list[dict]) -> dict:
    return {"name": name, "nodes": nodes, "edges": edges}


def layout_of(nodes: list[dict]) -> dict:
    return {
        item["id"]: {"x": 60 + index * 250, "y": 120 + (index % 2) * 160}
        for index, item in enumerate(nodes)
    }


def list_definitions(token: str) -> list[dict]:
    status, body = request("GET", GATEWAY + "/v1/workflows/definitions", token=token)
    if status != 200 or not isinstance(body, dict):
        return []
    return body.get("definitions") or []


def create_definition(token: str, name: str, document: dict, category: str, description: str) -> dict:
    status, body = request(
        "POST", GATEWAY + "/v1/workflows/definitions", token=token,
        payload={
            "name": name,
            "description": description,
            "category": category,
            "dag": document,
            "layout": layout_of(document["nodes"]),
        },
    )
    if status not in (200, 201) or not isinstance(body, dict):
        fail(f"create workflow {name} failed ({status}): {body}")
    return body["definition"]


def update_definition(token: str, definition: dict, document: dict) -> dict:
    status, body = request(
        "PUT", GATEWAY + f"/v1/workflows/definitions/{definition['id']}", token=token,
        payload={
            "name": definition["name"],
            "description": definition.get("description", ""),
            "category": definition.get("category", "custom"),
            "dag": document,
            "layout": layout_of(document["nodes"]),
        },
    )
    if status != 200 or not isinstance(body, dict):
        fail(f"update workflow {definition['name']} failed ({status}): {body}")
    return body["definition"]


def publish_definition(token: str, definition: dict) -> dict:
    status, body = request(
        "POST", GATEWAY + f"/v1/workflows/definitions/{definition['id']}/publish", token=token,
    )
    if status != 200 or not isinstance(body, dict):
        print(f"[platform] publish {definition['name']} skipped ({status}): {body}")
        return definition
    print(f"[platform] published {definition['name']} v{body.get('version')}")
    return body.get("definition") or definition


def unpublish_definition(token: str, definition: dict) -> dict:
    status, body = request(
        "POST", GATEWAY + f"/v1/workflows/definitions/{definition['id']}/unpublish", token=token,
    )
    if status != 200 or not isinstance(body, dict):
        return definition
    return body.get("definition") or definition


def seed_workflows(token: str, kb_id: str) -> dict[str, dict]:
    existing = {item.get("name"): item for item in list_definitions(token)}
    out: dict[str, dict] = {}

    base_nodes = [node("ingest", "ingest"), node("parse", "parse"), node("index", "index")]
    base_edges = [
        {"from": "ingest", "to": "parse"},
        {"from": "parse", "to": "index"},
    ]
    vector_name = "示例 · 文档向量化"
    if vector_name in existing:
        out["vector"] = existing[vector_name]
    else:
        definition = create_definition(
            token, vector_name, dag(vector_name, base_nodes, base_edges),
            "ingest", "声明式摄入/解析/索引；发布 v2 后附带真实检索节点",
        )
        definition = publish_definition(token, definition)
        definition = unpublish_definition(token, definition)
        nodes = base_nodes + [
            node("query", "query", {
                "query": "NexusFlow 的混合检索包含哪些召回路由？",
                "kb_id": kb_id, "top_k": 5, "final_top_n": 3,
            }),
        ]
        edges = base_edges + [{"from": "index", "to": "query"}]
        definition = update_definition(token, definition, dag(vector_name, nodes, edges))
        definition = publish_definition(token, definition)
        out["vector"] = definition

    chat_name = "示例 · 对话问答"
    if chat_name in existing:
        out["chat"] = existing[chat_name]
    else:
        nodes = [node("query", "query", {
            "query": "NexusFlow 的混合检索包含哪些召回路由？",
            "kb_id": kb_id, "top_k": 5, "final_top_n": 3,
        })]
        definition = create_definition(
            token, chat_name, dag(chat_name, nodes, []), "chat", "真实检索示例，可直接试运行",
        )
        out["chat"] = publish_definition(token, definition)

    approval_name = "示例 · 人工审批"
    if approval_name in existing:
        out["approval"] = existing[approval_name]
    else:
        nodes = [
            node("gate", "human_approval", {"prompt": "演示：请确认继续执行"}),
            node("after", "noop"),
        ]
        edges = [{"from": "gate", "to": "after"}]
        definition = create_definition(
            token, approval_name, dag(approval_name, nodes, edges),
            "pipeline", "运行到 gate 会挂起，可在审批中心批准后恢复",
        )
        out["approval"] = publish_definition(token, definition)

    condition_name = "示例 · 条件分流"
    condition_path = ROOT / "examples" / "demo" / "workflows" / "conditional-routing.json"
    if condition_name in existing:
        out["condition"] = existing[condition_name]
    elif condition_path.is_file():
        raw = json.loads(condition_path.read_text(encoding="utf-8"))
        document = raw.get("dag") or raw
        out["condition"] = create_definition(
            token, condition_name, document, "pipeline", "条件分支示例（草稿）",
        )

    failure_name = "示例 · 失败运行"
    if failure_name in existing:
        out["failure"] = existing[failure_name]
    else:
        nodes = [node("query", "query", {})]
        out["failure"] = create_definition(
            token, failure_name, dag(failure_name, nodes, []),
            "custom", "故意缺少 query 配置，用于展示失败运行记录",
        )
    return out


def seed_workflow_runs(token: str, definitions: dict[str, dict]) -> None:
    expectations = {"chat": 200, "approval": 202, "failure": 422}
    for key, expected in expectations.items():
        definition = definitions.get(key)
        if not definition:
            continue
        status, body = request(
            "GET",
            GATEWAY + f"/v1/workflows/runs?definition_id={definition['id']}&limit=5",
            token=token,
        )
        runs = (body.get("runs") or []) if isinstance(body, dict) else []
        if status == 200 and runs:
            print(f"[platform] workflow {definition['name']} already has runs")
            continue
        run_status, run_body = request(
            "POST", GATEWAY + "/v1/workflows/run", token=token,
            payload={
                "dag": definition["dag"],
                "definition_id": definition["id"],
                "input": {"source": "demo-seed"},
            },
            timeout=120,
        )
        state = (run_body or {}).get("status", "") if isinstance(run_body, dict) else ""
        print(
            f"[platform] run {definition['name']}: HTTP {run_status} status={state or '-'}"
        )
        if expected == 202 and run_status != 202:
            print("[platform] hint: enable HITL_ENABLED=true for the approval run")


def seed_subagent(token: str) -> None:
    status, body = request("GET", GATEWAY + "/v1/agents", token=token)
    agents = (body.get("agents") or []) if isinstance(body, dict) else []
    if any(agent.get("name") == "demo.chart" and agent.get("tenant_id") for agent in agents):
        print("[platform] SubAgent demo.chart already exists")
        return
    payload = {
        "name": "demo.chart",
        "kind": "sandbox",
        "capabilities": ["agent.call"],
        "skills": ["demo.chart"],
        "priority": 120,
        "timeout_ms": 5000,
        "risk": "execute",
        "sandbox": {
            "runtime": "python",
            "timeout_seconds": 10,
            "code": (
                "import json\n"
                "with open('/tmp/input.json') as f:\n"
                "    data = json.load(f)\n"
                "out = {'chart': 'bar', 'series': data.get('series', [1, 2, 3])}\n"
                "with open('/tmp/output.json', 'w') as f:\n"
                "    json.dump(out, f)\n"
                "print(json.dumps(out))\n"
            ),
        },
    }
    status, body = request("POST", GATEWAY + "/v1/agents", token=token, payload=payload)
    if status not in (200, 201):
        print(f"[platform] SubAgent seed skipped ({status}): {body}")
        return
    print("[platform] created sandbox SubAgent demo.chart")


def seed_webhooks(token: str) -> None:
    status, body = request("GET", GATEWAY + "/v1/webhooks", token=token)
    hooks = (body.get("webhooks") or []) if isinstance(body, dict) else []
    by_name = {str(hook.get("name")): hook for hook in hooks}
    target = "https://httpbin.org/post"
    if "demo-ops" not in by_name:
        status, body = request(
            "POST", GATEWAY + "/v1/webhooks", token=token,
            payload={
                "name": "demo-ops",
                "target_url": target,
                "events": ["task.completed", "workflow.completed"],
                "is_active": True,
            },
        )
        if status not in (200, 201):
            print(f"[platform] webhook demo-ops skipped ({status}): {body}")
        else:
            by_name["demo-ops"] = (body or {}).get("webhook") or {}
            print("[platform] created webhook demo-ops (secret shown once below)")
            secret = (body or {}).get("secret") or ""
            if secret:
                print(f"            secret: {secret}")
    if "demo-paused" not in by_name:
        status, body = request(
            "POST", GATEWAY + "/v1/webhooks", token=token,
            payload={
                "name": "demo-paused",
                "target_url": target,
                "events": ["approval.decided"],
                "is_active": False,
            },
        )
        if status in (200, 201):
            print("[platform] created paused webhook demo-paused")
    hook = by_name.get("demo-ops") or {}
    hook_id = str(hook.get("id") or "")
    if not hook_id:
        return
    status, body = request(
        "GET", GATEWAY + f"/v1/webhooks/{hook_id}/deliveries?limit=5", token=token,
    )
    deliveries = (body.get("deliveries") or []) if isinstance(body, dict) else []
    if deliveries:
        print(f"[platform] webhook demo-ops already has {len(deliveries)} delivery record(s)")
        return
    request("POST", GATEWAY + f"/v1/webhooks/{hook_id}/test", token=token)
    for _ in range(4):
        time.sleep(2)
        status, body = request(
            "GET", GATEWAY + f"/v1/webhooks/{hook_id}/deliveries?limit=5", token=token,
        )
        deliveries = (body.get("deliveries") or []) if isinstance(body, dict) else []
        if deliveries:
            print(f"[platform] webhook test delivery: {deliveries[0].get('status')}")
            return
    print("[platform] webhook test scheduled; delivery log may appear shortly")


def seed_extra_api_keys(token: str, kb_id: str) -> None:
    status, body = request("GET", GATEWAY + "/v1/api-keys", token=token)
    keys = (body.get("api_keys") or []) if isinstance(body, dict) else []
    by_name = {str(key.get("name")): key for key in keys}
    if "demo-search" not in by_name:
        status, body = request(
            "POST", GATEWAY + "/v1/api-keys", token=token,
            payload={
                "name": "demo-search",
                "scopes": ["search:use"],
                "allowed_kb_ids": [kb_id],
                "rate_limit_rpm": 120,
            },
        )
        if status in (200, 201):
            print("[platform] created API key demo-search (plaintext shown once below)")
            plain = (body or {}).get("key") or ""
            if plain:
                print(f"            key: {plain}")
        else:
            print(f"[platform] API key demo-search skipped ({status}): {body}")
    if "demo-revoked" not in by_name:
        status, body = request(
            "POST", GATEWAY + "/v1/api-keys", token=token,
            payload={
                "name": "demo-revoked",
                "scopes": ["search:use"],
                "allowed_kb_ids": [kb_id],
                "rate_limit_rpm": 30,
            },
        )
        key_id = str(((body or {}).get("api_key") or {}).get("id") or "")
        if status in (200, 201) and key_id:
            request("DELETE", GATEWAY + f"/v1/api-keys/{key_id}", token=token)
            print("[platform] created and revoked API key demo-revoked")


def seed_durable_approvals_and_audit(user_id: str) -> None:
    """Inserts long-lived demo approvals and audit events (compose Postgres)."""
    if not shutil.which("docker"):
        print("[platform] docker not found; skipping durable approvals/audit")
        return
    uid = user_id or "demo-admin"
    sql = f"""
    DELETE FROM audit_logs WHERE id LIKE 'aud-demo-%';

    INSERT INTO agent_approvals (
        id, tenant_id, tool, args, risk, user_id, trace_id,
        status, reason, decided_by, created_at, decided_at, expires_at
    ) VALUES
      ('apr-demo-pending', 'default', 'sandbox.execute',
       '{{"code":"print(42)","runtime":"python"}}'::jsonb, 'execute',
       '{uid}', 'demo-trace-tool', 'pending', '', '',
       NOW() - interval '3 minutes', NULL, NOW() + interval '365 days'),
      ('apr-demo-approved', 'default', 'workflow.run',
       '{{"dag":"demo"}}'::jsonb, 'execute',
       '{uid}', 'demo-trace-approved', 'approved', '演示：批准', '{uid}',
       NOW() - interval '20 minutes', NOW() - interval '19 minutes', NOW() + interval '365 days'),
      ('apr-demo-rejected', 'default', 'sandbox.execute',
       '{{"code":"rm -rf /"}}'::jsonb, 'execute',
       '{uid}', 'demo-trace-rejected', 'rejected', '演示：拒绝', '{uid}',
       NOW() - interval '40 minutes', NOW() - interval '39 minutes', NOW() + interval '365 days')
    ON CONFLICT (id) DO UPDATE SET
      status = EXCLUDED.status, reason = EXCLUDED.reason,
      decided_by = EXCLUDED.decided_by, decided_at = EXCLUDED.decided_at,
      expires_at = EXCLUDED.expires_at;

    INSERT INTO audit_logs (
        id, tenant_id, user_id, action, resource_type, resource_id,
        trace_id, pii_masked_count, status_code, metadata, created_at
    ) VALUES
      ('aud-demo-tool', 'default', '{uid}', 'tool.call', 'tool', 'search.query',
       'demo-trace-search', 0, 200,
       '{{"risk":"read","decision":"allowed","status":"ok","detail":{{"query":"NexusFlow"}}}}'::jsonb,
       NOW() - interval '5 minutes'),
      ('aud-demo-pending', 'default', '{uid}', 'approval.create', 'approval', 'sandbox.execute',
       'demo-trace-tool', 0, 202,
       '{{"risk":"execute","decision":"pending","status":"created","detail":{{"approval_id":"apr-demo-pending"}}}}'::jsonb,
       NOW() - interval '3 minutes'),
      ('aud-demo-approved', 'default', '{uid}', 'approval.decide', 'approval', 'workflow.run',
       'demo-trace-approved', 0, 200,
       '{{"risk":"execute","decision":"allowed","status":"approved","detail":{{"approval_id":"apr-demo-approved"}}}}'::jsonb,
       NOW() - interval '19 minutes'),
      ('aud-demo-rejected', 'default', '{uid}', 'approval.decide', 'approval', 'sandbox.execute',
       'demo-trace-rejected', 0, 403,
       '{{"risk":"execute","decision":"denied","status":"rejected","detail":{{"approval_id":"apr-demo-rejected"}}}}'::jsonb,
       NOW() - interval '39 minutes'),
      ('aud-demo-pii', 'default', '{uid}', 'pii.mask', 'agent', 'chat',
       'demo-trace-pii', 2, 200,
       '{{"risk":"read","decision":"allowed","status":"ok","detail":{{"pii_masked_count":2}}}}'::jsonb,
       NOW() - interval '10 minutes')
    ON CONFLICT DO NOTHING;
    """
    result = subprocess.run(
        [
            "docker", "exec", PG_CONTAINER, "psql", "-U", PG_USER, "-d", PG_DB,
            "-v", "ON_ERROR_STOP=1", "-c", sql,
        ],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print(f"[platform] durable approvals/audit skipped: {result.stderr.strip()[:200]}")
        return
    print("[platform] seeded demo approvals and audit events")


def seed_default_chat(token: str) -> None:
    status, body = request(
        "GET", GATEWAY + "/v1/conversations?limit=1&offset=0&kb_id=default", token=token,
    )
    total = int(body.get("total") or 0) if isinstance(body, dict) else 0
    if status == 200 and total > 0:
        print("[platform] default KB already has conversations")
        return
    sample_chat(token, "default")


def seed_platform(token: str, kb_id: str) -> None:
    ensure_default_doc(token)
    ensure_reference_kb(token, kb_id)
    definitions = seed_workflows(token, kb_id)
    seed_workflow_runs(token, definitions)
    seed_subagent(token)
    seed_webhooks(token)
    seed_extra_api_keys(token, kb_id)
    seed_durable_approvals_and_audit(me(token))
    seed_default_chat(token)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kb-name", default=DEMO_KB_NAME)
    parser.add_argument("--password", default=os.environ.get("NF_DEMO_PASSWORD", DEMO_PASSWORD_DEFAULT))
    parser.add_argument("--skip-users", action="store_true")
    parser.add_argument("--skip-api-key", action="store_true")
    parser.add_argument("--skip-check", action="store_true")
    parser.add_argument("--skip-platform", action="store_true")
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

    if not args.skip_platform:
        print("\n[platform] seeding workflows, approvals, webhooks, agents and audit")
        seed_platform(token, kb_id)

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
