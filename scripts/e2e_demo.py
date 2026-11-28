"""Runs one document through the entire NexusFlow pipeline and records evidence.

Flow: login -> knowledge base -> upload PDF -> MinerU parse -> layout artifact ->
Qdrant vectors -> LlamaIndex routes -> Gateway hybrid search -> RAG chat with
citations -> LightRAG graph -> workflow loop children -> HITL approval -> MCP.

Only the Python standard library is required.

Usage:
    python scripts/e2e_demo.py
    python scripts/e2e_demo.py --output docs/e2e-evidence.md
"""
from __future__ import annotations

import argparse
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
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


GATEWAY = os.environ.get("NF_GATEWAY", f"http://127.0.0.1:{env_value('GATEWAY_PUBLISH_PORT', '8080')}")
LLAMAINDEX = os.environ.get("NF_LLAMAINDEX", "http://127.0.0.1:8000")
QDRANT = os.environ.get("NF_QDRANT", "http://127.0.0.1:6333")
HUB = os.environ.get("NF_HUB", "http://127.0.0.1:8001")
HUB_TOKEN = os.environ.get("NF_HUB_TOKEN", env_value("AGENT_HUB_ADMIN_TOKEN", "nexusflow-agent-admin"))
ADMIN = env_value("SEED_ADMIN_USERNAME", "admin")
PASSWORD = env_value("SEED_ADMIN_PASSWORD", "admin123")


def check(name: str, ok: bool, detail: str = "") -> bool:
    RESULTS.append(("PASS" if ok else "FAIL", name, detail))
    print(f"{'PASS' if ok else 'FAIL'} {name}" + (f" | {detail}" if detail else ""), flush=True)
    return ok


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
    except Exception as exc:  # noqa: BLE001
        return 0, f"{type(exc).__name__}: {exc}"
    try:
        return status, json.loads(body)
    except json.JSONDecodeError:
        return status, body


def minimal_pdf(marker: str) -> bytes:
    text = (
        f"BT /F1 12 Tf 72 720 Td (NexusFlow end to end evidence {marker}) Tj "
        "0 -24 Td (This page proves MinerU parsing, chunking, embedding and retrieval.) Tj ET"
    ).encode()
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length " + str(len(text)).encode() + b" >>\nstream\n" + text + b"\nendstream",
    ]
    document = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = [0]
    for number, obj in enumerate(objects, 1):
        offsets.append(len(document))
        document.extend(f"{number} 0 obj\n".encode() + obj + b"\nendobj\n")
    xref = len(document)
    document.extend(f"xref\n0 {len(objects) + 1}\n".encode() + b"0000000000 65535 f \n")
    for offset in offsets[1:]:
        document.extend(f"{offset:010d} 00000 n \n".encode())
    document.extend(
        f"trailer\n<< /Size {len(objects) + 1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n".encode()
    )
    return bytes(document)


def multipart(filename: str, content: bytes, kb_id: str) -> tuple[bytes, str]:
    boundary = "----nexusflow-e2e-" + uuid.uuid4().hex
    body = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        "Content-Type: application/pdf\r\n\r\n"
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
            return str(item.get("id"))
    status, body = request(
        "POST", GATEWAY + "/v1/knowledge-bases", token=token,
        payload={"name": name, "description": "端到端流程验证（可删除）"},
    )
    kb_id = str(((body or {}).get("knowledge_base") or {}).get("id") or "")
    if status != 200 or not kb_id:
        raise SystemExit(f"create knowledge base failed: {status} {body}")
    return kb_id


def wait_task(token: str, task_id: str, kb_id: str) -> dict:
    deadline = time.time() + 240
    task: dict = {}
    while time.time() < deadline:
        status, body = request(
            "GET", GATEWAY + f"/v1/tasks/{task_id}?kb_id={urllib.parse.quote(kb_id)}", token=token,
        )
        if isinstance(body, dict):
            task = body
        state = str(task.get("status", ""))
        if state in {"COMPLETED", "FAILED"}:
            return task
        time.sleep(4)
    return task


def qdrant_points(kb_id: str, task_id: str) -> list[dict]:
    status, body = request("POST", QDRANT + f"/collections/nexusflow_{kb_id}/points/scroll", payload={
        "filter": {"must": [{"key": "task_id", "match": {"value": task_id}}]},
        "limit": 20, "with_payload": True, "with_vector": True,
    })
    return ((body or {}).get("result") or {}).get("points", []) if isinstance(body, dict) else []


def vector_size(point: dict) -> int:
    vector = point.get("vector")
    if isinstance(vector, list):
        return len(vector)
    if isinstance(vector, dict):
        return sum(len(item) for item in vector.values() if isinstance(item, list))
    return 0


def llama_search(payload: dict) -> tuple[int, list]:
    status, body = request("POST", LLAMAINDEX + "/v1/search", payload=payload, timeout=180)
    return status, (body.get("chunks", []) if isinstance(body, dict) else [])


def mcp_checks() -> None:
    headers = {"x-admin-token": HUB_TOKEN, "Content-Type": "application/json"}
    init = {"jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                       "clientInfo": {"name": "e2e-demo"}, "_meta": {"tenantId": "default"}}}
    req = urllib.request.Request(HUB + "/mcp", data=json.dumps(init).encode(), method="POST", headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            session = resp.headers.get("mcp-session-id", "")
            status = resp.status
    except urllib.error.HTTPError as exc:
        check("MCP initialize", False, f"HTTP {exc.code} (need AGENT_HUB_MCP_ENABLED=true)")
        return
    check("MCP initialize", status == 200 and bool(session), f"session={session}")
    if not session:
        return
    session_headers = {**headers, "mcp-session-id": session}
    req = urllib.request.Request(HUB + "/mcp", data=json.dumps(
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {"_meta": {"tenantId": "default"}}}
    ).encode(), method="POST", headers=session_headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            tools = (json.loads(resp.read().decode()).get("result") or {}).get("tools", [])
        check("MCP tools/list", len(tools) > 0, f"tools={[t.get('name') for t in tools]}")
    except urllib.error.HTTPError as exc:
        check("MCP tools/list", False, f"HTTP {exc.code}")


def workflow_checks(token: str) -> None:
    loop_dag = {
        "name": "e2e-loop",
        "nodes": [{"id": "loop", "type": "loop", "config": {
            "items": ["a", "b", "c"], "max_concurrency": 2,
            "sub_dag": {"name": "inner", "nodes": [{"id": "noop", "type": "noop"}], "edges": []},
        }}],
        "edges": [],
    }
    status, body = request("POST", GATEWAY + "/v1/workflows/run", token=token, payload={"dag": loop_dag})
    run_id = str((body or {}).get("run_id") or "") if isinstance(body, dict) else ""
    check("workflow loop run", status == 200 and bool(run_id), f"run={run_id} status={(body or {}).get('status')}")
    if run_id:
        status, body = request("GET", f"{GATEWAY}/v1/workflows/runs/{run_id}/children?limit=10", token=token)
        children = body.get("runs", []) if isinstance(body, dict) else []
        check("loop child runs persisted", status == 200 and len(children) == 3, f"children={len(children)}")

    approval_dag = {
        "name": "e2e-approval",
        "nodes": [{"id": "gate", "type": "human_approval"}, {"id": "after", "type": "noop"}],
        "edges": [{"from": "gate", "to": "after"}],
    }
    status, body = request("POST", GATEWAY + "/v1/workflows/run", token=token, payload={"dag": approval_dag})
    approval_run = str((body or {}).get("run_id") or "") if isinstance(body, dict) else ""
    check("approval run suspends", status == 202 and bool(approval_run), f"run={approval_run}")
    if approval_run:
        status, body = request("POST", f"{GATEWAY}/v1/workflows/runs/{approval_run}/approve", token=token,
                               payload={"decision": "approve", "reason": "e2e evidence"})
        check("approval resumes to COMPLETED", status == 200 and (body or {}).get("status") == "COMPLETED",
              f"status={(body or {}).get('status')}")
        status, _ = request("POST", f"{GATEWAY}/v1/workflows/runs/{approval_run}/approve", token=token,
                            payload={"decision": "approve"})
        check("duplicate decision returns 409", status == 409, f"status={status}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kb-name", default="全流程验证")
    parser.add_argument("--output", default="")
    parser.add_argument("--skip-workflow", action="store_true")
    parser.add_argument("--skip-mcp", action="store_true")
    args = parser.parse_args()

    started = time.strftime("%Y-%m-%d %H:%M:%S")
    marker = "NF-E2E-" + uuid.uuid4().hex[:10]
    filename = f"e2e-{marker}.pdf"

    status, body = request("POST", GATEWAY + "/v1/auth/login",
                           payload={"username": ADMIN, "password": PASSWORD}, timeout=30)
    token = (body or {}).get("access_token", "") if isinstance(body, dict) else ""
    if not check("admin login", status == 200 and bool(token), f"status={status}"):
        return 2

    kb_id = ensure_kb(token, args.kb_name)
    check("knowledge base ready", bool(kb_id), f"name={args.kb_name} id={kb_id}")

    raw, ctype = multipart(filename, minimal_pdf(marker), kb_id)
    status, body = request("POST", GATEWAY + "/v1/files", token=token, raw=raw, content_type=ctype, timeout=180)
    task_id = str((body or {}).get("task_id") or "") if isinstance(body, dict) else ""
    if not check("upload accepted", status == 202 and bool(task_id), f"task={task_id}"):
        return 2

    task = wait_task(token, task_id, kb_id)
    meta = task.get("metadata") or {}
    check("MinerU parse completed", task.get("status") == "COMPLETED", f"status={task.get('status')}")
    layout_key = str(meta.get("layout_key") or "")
    check("layout.json persisted", bool(layout_key), f"key={layout_key}")
    layout_bytes = 0
    if layout_key:
        status, body = request(
            "GET", GATEWAY + "/v1/media?key=" + urllib.parse.quote(layout_key, safe=""), token=token,
        )
        layout_bytes = len(json.dumps(body)) if isinstance(body, (dict, list)) else 0
        check("layout.json readable", status == 200 and layout_bytes > 0, f"bytes={layout_bytes}")

    points = qdrant_points(kb_id, task_id)
    dims = [vector_size(point) for point in points]
    check("Qdrant vectors written", bool(points) and all(d > 0 for d in dims),
          f"points={len(points)} dims={dims[:3]}")
    tenants = sorted({str((point.get("payload") or {}).get("tenant_id")) for point in points})
    check("vectors carry tenant", tenants == ["default"], f"tenants={tenants}")

    queries = {"query": marker, "kb_id": kb_id, "final_top_n": 5,
               "filters": {"tenant_id": "default", "task_id": task_id}}
    route_counts: dict[str, int] = {}
    for route in ("vector", "bm25"):
        status, chunks = llama_search({**queries, "routes": [{"type": route, "top_k": 5}]})
        route_counts[route] = len(chunks)
        check(f"llama {route} route", status == 200 and len(chunks) > 0, f"chunks={len(chunks)}")

    lightrag_hit = False
    graph_chunks: list = []
    deadline = time.time() + 180
    while time.time() < deadline:
        status, graph_chunks = llama_search({**queries, "routes": [{"type": "graph", "top_k": 5}]})
        lightrag_hit = any((chunk.get("meta") or {}).get("retriever") == "lightrag" for chunk in graph_chunks)
        if status == 200 and graph_chunks and lightrag_hit:
            break
        time.sleep(5)
    route_counts["graph"] = len(graph_chunks)
    check("graph route served by LightRAG", graph_chunks and lightrag_hit,
          f"chunks={len(graph_chunks)} retriever=lightrag")

    status, hybrid_body = request("POST", GATEWAY + "/v1/search", token=token, payload={
        "query": marker, "kb_id": kb_id, "final_top_n": 5,
        "routes": [{"type": "vector", "top_k": 10}, {"type": "bm25", "top_k": 10}, {"type": "graph", "top_k": 10}],
    })
    gateway_chunks = (hybrid_body or {}).get("chunks", []) if isinstance(hybrid_body, dict) else []
    check("gateway hybrid search", status == 200 and bool(gateway_chunks),
          f"returned={len(gateway_chunks)} recall_count={(hybrid_body or {}).get('recall_count')}")

    status, body = request("POST", GATEWAY + "/v1/chat", token=token, payload={
        "message": f"文档 {filename} 里的验证标记是什么？", "kb_id": kb_id,
    }, timeout=180)
    citations = (body or {}).get("citations", []) if isinstance(body, dict) else []
    answer = str((body or {}).get("answer", "")).replace("\n", " ")
    cites_task = any(
        (citation.get("meta") or {}).get("task_id") == task_id or marker in str(citation.get("content", ""))
        for citation in citations
    )
    check("RAG chat answers with citations", status == 200 and bool(citations) and cites_task,
          f"citations={len(citations)} hits_uploaded_doc={cites_task}")
    answer = answer[:160]

    if not args.skip_workflow:
        workflow_checks(token)
    if not args.skip_mcp:
        mcp_checks()

    passed = sum(1 for status, _, _ in RESULTS if status == "PASS")
    failed = [item for item in RESULTS if item[0] == "FAIL"]
    print(f"\n== {passed} passed, {len(failed)} failed ==", flush=True)

    lines = [
        "# NexusFlow 全流程验证证据",
        "",
        "> 由 `python scripts/e2e_demo.py --output docs/e2e-evidence.md` 生成；以下 ID 为本地环境实测值。",
        "",
        f"- 执行时间：{started}",
        f"- Gateway：`{GATEWAY}`",
        f"- 验证知识库：`{args.kb_name}`（`{kb_id}`）",
        f"- 上传文档：`{filename}`",
        f"- 任务 ID：`{task_id}`",
        f"- 解析结果：`{task.get('status')}`，layout：`{layout_key}`（{layout_bytes} 字节）",
        f"- Qdrant：`{len(points)}` 个点，向量维度 `{dims[:1]}`，租户 `{tenants}`",
        f"- 路由召回：vector={route_counts.get('vector', 0)}，bm25={route_counts.get('bm25', 0)}，graph={route_counts.get('graph', 0)}（retriever=lightrag）",
        f"- Gateway 混合检索：返回 `{len(gateway_chunks)}` 条，recall_count=`{(hybrid_body or {}).get('recall_count') if isinstance(hybrid_body, dict) else ''}`",
        f"- 问答引用：`{len(citations)}` 条，命中上传文档：`{cites_task}`",
        f"- 回答摘录：{answer}",
        "",
        "## 检查项",
        "",
        "| 结果 | 检查项 | 细节 |",
        "| --- | --- | --- |",
    ]
    for status, name, detail in RESULTS:
        lines.append(f"| {status} | {name} | {detail.replace('|', '/')} |")
    report = "\n".join(lines) + "\n"

    if args.output:
        path = Path(args.output)
        if not path.is_absolute():
            path = ROOT / path
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(report, encoding="utf-8")
        print(f"evidence written to {path}", flush=True)
    return 0 if not failed else 2


if __name__ == "__main__":
    raise SystemExit(main())
