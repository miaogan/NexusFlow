import type {
  Dag,
  MeInfo,
  SandboxExecuteRequest,
  SandboxResult,
  SearchResponse,
  TaskDetail,
  TaskListResponse,
  UploadResult,
  ValidationResult,
} from "../types";

const GATEWAY_BASE = import.meta.env.VITE_GATEWAY_URL ?? "/api";
const WORKFLOW_BASE = import.meta.env.VITE_WORKFLOW_URL ?? "/wf";
const TOKEN_KEY = "nexusflow_token";

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string | null) {
  if (token) localStorage.setItem(TOKEN_KEY, token);
  else localStorage.removeItem(TOKEN_KEY);
}

function authHeaders(): HeadersInit {
  const token = getToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

async function parse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const detail = await response.text();
    let message = detail || `HTTP ${response.status}`;
    try {
      const parsed = JSON.parse(detail);
      if (parsed?.error) message = parsed.error;
    } catch {
      /* keep raw text */
    }
    throw new Error(message);
  }
  return (await response.json()) as T;
}

async function request<T>(url: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers({ ...authHeaders(), ...(init.headers as Record<string, string>) });
  const response = await fetch(url, { ...init, headers });
  return parse<T>(response);
}

// ---------- Gateway APIs ----------

export async function uploadFile(
  file: File,
  metadata?: Record<string, string>,
): Promise<UploadResult> {
  const form = new FormData();
  form.append("file", file);
  for (const [key, value] of Object.entries(metadata ?? {})) {
    form.append(key, value);
  }
  // Do not set Content-Type manually; the browser sets the multipart boundary.
  const headers = new Headers(authHeaders());
  const response = await fetch(`${GATEWAY_BASE}/v1/files`, {
    method: "POST",
    headers,
    body: form,
  });
  return parse<UploadResult>(response);
}

export async function listTasks(
  status?: string,
  limit = 50,
  offset = 0,
): Promise<TaskListResponse> {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (status) params.set("status", status);
  return request<TaskListResponse>(`${GATEWAY_BASE}/v1/tasks?${params.toString()}`);
}

export async function getTask(id: string): Promise<TaskDetail> {
  return request<TaskDetail>(`${GATEWAY_BASE}/v1/tasks/${encodeURIComponent(id)}`);
}

export async function deleteTask(id: string): Promise<{ success: boolean }> {
  return request<{ success: boolean }>(`${GATEWAY_BASE}/v1/tasks/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export async function retryTask(id: string): Promise<{ task_id: string; status: string; message: string }> {
  return request(`${GATEWAY_BASE}/v1/tasks/${encodeURIComponent(id)}/retry`, {
    method: "POST",
  });
}

export async function runSearch(query: string): Promise<SearchResponse> {
  return request<SearchResponse>(`${GATEWAY_BASE}/v1/search`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      query,
      routes: [
        { type: "vector", top_k: 20 },
        { type: "bm25", top_k: 20 },
      ],
      final_top_n: 5,
    }),
  });
}

export async function executeSandbox(req: SandboxExecuteRequest): Promise<SandboxResult> {
  return request<SandboxResult>(`${GATEWAY_BASE}/v1/sandbox/execute`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
}

export async function getMe(): Promise<MeInfo> {
  return request<MeInfo>(`${GATEWAY_BASE}/v1/me`);
}

// ---------- Workflow Engine APIs ----------

export async function listNodeTypes(): Promise<{ nodes: string[] }> {
  return request<{ nodes: string[] }>(`${WORKFLOW_BASE}/v1/nodes`);
}

export async function validateDag(dag: Dag): Promise<ValidationResult> {
  return request<ValidationResult>(`${WORKFLOW_BASE}/v1/workflows/validate`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dag }),
  });
}

export async function runDag(dag: Dag): Promise<unknown> {
  return request<unknown>(`${WORKFLOW_BASE}/v1/workflows/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dag, input: { source: "web-console" } }),
  });
}
