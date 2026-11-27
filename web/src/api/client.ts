import type {
  AuthUser,
  ChatResponse,
  Citation,
  ConversationDetail,
  ConversationListResponse,
  Dag,
  IntentInfo,
  JwtClaims,
  KnowledgeBase,
  LoginResponse,
  MeInfo,
  PermissionInfo,
  RoleInfo,
  SandboxExecuteRequest,
  SandboxResult,
  SearchResponse,
  TaskDetail,
  TaskListResponse,
  Trace,
  TraceListResponse,
  UploadResult,
  UsageResponse,
  UserListResponse,
  ValidationResult,
} from "../types";

const GATEWAY_BASE = import.meta.env.VITE_GATEWAY_URL ?? "/api";
const WORKFLOW_BASE = import.meta.env.VITE_WORKFLOW_URL ?? "/wf";
const TOKEN_KEY = "nexusflow_token";
const REFRESH_KEY = "nexusflow_refresh_token";
const USER_KEY = "nexusflow_user";
const ANON_KEY = "nexusflow_anon";
const KB_KEY = "nexusflow_kb";

/** The knowledge base currently selected in the console (defaults to "default"). */
export function getActiveKB(): string {
  return localStorage.getItem(KB_KEY) || "default";
}

export function setActiveKB(id: string) {
  if (id) localStorage.setItem(KB_KEY, id);
  else localStorage.removeItem(KB_KEY);
}

/** Query-string suffix scoping a request to a knowledge base. */
function kbQuery(kbID: string): string {
  return `?kb_id=${encodeURIComponent(kbID)}`;
}

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string | null) {
  if (token) localStorage.setItem(TOKEN_KEY, token);
  else localStorage.removeItem(TOKEN_KEY);
}

export function getRefreshToken(): string | null {
  return localStorage.getItem(REFRESH_KEY);
}

export function getCurrentUser(): AuthUser | null {
  const raw = localStorage.getItem(USER_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as AuthUser;
  } catch {
    return null;
  }
}

/** Persist a login/refresh result: access token, refresh token and profile. */
export function setAuth(res: LoginResponse) {
  setToken(res.access_token);
  localStorage.setItem(REFRESH_KEY, res.refresh_token);
  localStorage.setItem(USER_KEY, JSON.stringify(res.user));
  localStorage.removeItem(ANON_KEY);
}

export function clearAuth() {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(REFRESH_KEY);
  localStorage.removeItem(USER_KEY);
  localStorage.removeItem(ANON_KEY);
}

export function setAnonymous(on: boolean) {
  if (on) localStorage.setItem(ANON_KEY, "1");
  else localStorage.removeItem(ANON_KEY);
}

export function isAnonymous(): boolean {
  return localStorage.getItem(ANON_KEY) === "1";
}

/** Decode the current access token's payload (no signature check; display only). */
export function getClaims(): JwtClaims | null {
  const token = getToken();
  if (!token) return null;
  const parts = token.split(".");
  if (parts.length < 2) return null;
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
    const bin = atob(padded);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return JSON.parse(new TextDecoder().decode(bytes)) as JwtClaims;
  } catch {
    return null;
  }
}

export function hasPermission(perm: string): boolean {
  const perms = getClaims()?.permissions ?? [];
  return perms.includes("*") || perms.includes(perm);
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

// Endpoints that must not trigger a refresh/retry loop.
const NO_REFRESH = ["/v1/auth/login", "/v1/auth/register", "/v1/auth/refresh"];

let refreshing: Promise<boolean> | null = null;

// tryRefresh exchanges the stored refresh token for a new pair. Concurrent
// callers share one in-flight refresh. On failure it clears auth and redirects.
async function tryRefresh(): Promise<boolean> {
  const rt = getRefreshToken();
  if (!rt) return false;
  if (refreshing) return refreshing;

  const p = (async () => {
    try {
      const res = await fetch(`${GATEWAY_BASE}/v1/auth/refresh`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ refresh_token: rt }),
      });
      if (!res.ok) {
        clearAuth();
        if (!window.location.pathname.startsWith("/login")) window.location.assign("/login");
        return false;
      }
      setAuth((await res.json()) as LoginResponse);
      return true;
    } catch {
      return false;
    }
  })();

  refreshing = p;
  try {
    return await p;
  } finally {
    if (refreshing === p) refreshing = null;
  }
}

async function request<T>(url: string, init: RequestInit = {}): Promise<T> {
  const send = () =>
    fetch(url, {
      ...init,
      headers: new Headers({ ...authHeaders(), ...(init.headers as Record<string, string>) }),
    });

  let response = await send();
  if (response.status === 401 && !NO_REFRESH.some((p) => url.includes(p))) {
    if (await tryRefresh()) {
      response = await send();
    }
  }
  return parse<T>(response);
}

// ---------- Gateway APIs ----------

export async function uploadFile(
  file: File,
  metadata?: Record<string, string>,
  kbID = getActiveKB(),
): Promise<UploadResult> {
  const form = new FormData();
  form.append("file", file);
  form.append("kb_id", kbID);
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
  kbID = getActiveKB(),
): Promise<TaskListResponse> {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (status) params.set("status", status);
  params.set("kb_id", kbID);
  return request<TaskListResponse>(`${GATEWAY_BASE}/v1/tasks?${params.toString()}`);
}

export async function getTask(id: string, kbID = getActiveKB()): Promise<TaskDetail> {
  return request<TaskDetail>(
    `${GATEWAY_BASE}/v1/tasks/${encodeURIComponent(id)}${kbQuery(kbID)}`,
  );
}

export async function deleteTask(
  id: string,
  kbID = getActiveKB(),
): Promise<{ success: boolean }> {
  return request<{ success: boolean }>(
    `${GATEWAY_BASE}/v1/tasks/${encodeURIComponent(id)}${kbQuery(kbID)}`,
    { method: "DELETE" },
  );
}

/** Cascade file delete: object storage, vectors/graph nodes and task record. */
export async function deleteFile(
  id: string,
  kbID = getActiveKB(),
): Promise<{ success: boolean; object_deleted?: boolean; index_deleted?: number }> {
  return request(`${GATEWAY_BASE}/v1/files/${encodeURIComponent(id)}${kbQuery(kbID)}`, {
    method: "DELETE",
  });
}

export async function retryTask(
  id: string,
  kbID = getActiveKB(),
): Promise<{ task_id: string; status: string; message: string }> {
  return request(
    `${GATEWAY_BASE}/v1/tasks/${encodeURIComponent(id)}/retry${kbQuery(kbID)}`,
    { method: "POST" },
  );
}

export async function runSearch(
  query: string,
  kbID = getActiveKB(),
): Promise<SearchResponse> {
  return request<SearchResponse>(`${GATEWAY_BASE}/v1/search`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      query,
      kb_id: kbID,
      routes: [
        { type: "vector", top_k: 20 },
        { type: "bm25", top_k: 20 },
      ],
      final_top_n: 5,
    }),
  });
}

// ---------- RAG chat APIs ----------

export async function chat(
  message: string,
  conversationId?: string,
  topK?: number,
  kbID = getActiveKB(),
): Promise<ChatResponse> {
  return request<ChatResponse>(`${GATEWAY_BASE}/v1/chat`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      message,
      conversation_id: conversationId || undefined,
      top_k: topK,
      kb_id: kbID,
    }),
  });
}

export interface ChatStreamHandlers {
  onStage?: (stage: string, status: string, count?: number) => void;
  onIntent?: (intent: IntentInfo) => void;
  onSources?: (citations: Citation[]) => void;
  onToken?: (text: string) => void;
  onDone?: (res: ChatResponse) => void;
  onError?: (message: string) => void;
}

/**
 * Streams a chat turn over SSE (POST + ReadableStream). Events:
 * stage → sources → token* → done, or error.
 */
export async function chatStream(
  message: string,
  conversationId: string | undefined,
  handlers: ChatStreamHandlers,
  kbID = getActiveKB(),
): Promise<void> {
  const send = () =>
    fetch(`${GATEWAY_BASE}/v1/chat/stream`, {
      method: "POST",
      headers: new Headers({ ...authHeaders(), "Content-Type": "application/json" }),
      body: JSON.stringify({
        message,
        conversation_id: conversationId || undefined,
        kb_id: kbID,
      }),
    });

  let response = await send();
  if (response.status === 401 && (await tryRefresh())) {
    response = await send();
  }
  if (!response.ok) {
    const detail = await response.text();
    let msg = detail || `HTTP ${response.status}`;
    try {
      const parsed = JSON.parse(detail);
      if (parsed?.error) msg = parsed.error;
    } catch {
      /* keep raw text */
    }
    throw new Error(msg);
  }
  if (!response.body) throw new Error("streaming unsupported by this browser");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      let idx = buffer.indexOf("\n\n");
      while (idx >= 0) {
        const block = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 2);
        for (const line of block.split("\n")) {
          if (!line.startsWith("data:")) continue;
          const payload = line.slice(5).trim();
          if (!payload) continue;
          let event: Record<string, unknown>;
          try {
            event = JSON.parse(payload) as Record<string, unknown>;
          } catch {
            continue;
          }
          switch (event.type) {
            case "stage":
              handlers.onStage?.(
                String(event.stage ?? ""),
                String(event.status ?? ""),
                typeof event.count === "number" ? event.count : undefined,
              );
              break;
            case "intent":
              handlers.onIntent?.(event as unknown as IntentInfo);
              break;
            case "sources":
              handlers.onSources?.((event.citations as Citation[]) ?? []);
              break;
            case "token":
              handlers.onToken?.(String(event.content ?? ""));
              break;
            case "done":
              handlers.onDone?.(event as unknown as ChatResponse);
              break;
            case "error": {
              const msg = String(event.error ?? "stream error");
              handlers.onError?.(msg);
              throw new Error(msg);
            }
          }
        }
        idx = buffer.indexOf("\n\n");
      }
    }
  } finally {
    reader.releaseLock();
  }
}

export async function listConversations(
  limit = 20,
  offset = 0,
): Promise<ConversationListResponse> {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  return request<ConversationListResponse>(
    `${GATEWAY_BASE}/v1/conversations?${params.toString()}`,
  );
}

export async function getConversation(id: string): Promise<ConversationDetail> {
  return request<ConversationDetail>(
    `${GATEWAY_BASE}/v1/conversations/${encodeURIComponent(id)}`,
  );
}

// ---------- Observability APIs ----------

export interface FeedbackInput {
  trace_id?: string;
  message_id?: string;
  conversation_id?: string;
  value?: number;
  name?: string;
  comment?: string;
  corrected_answer?: string;
  query?: string;
  answer?: string;
  cited_chunk_ids?: string[];
  meta?: Record<string, string>;
}

export async function submitFeedback(
  input: FeedbackInput,
): Promise<{ ok: boolean; id?: string }> {
  return request<{ ok: boolean; id?: string }>(`${GATEWAY_BASE}/v1/feedback`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function listTraces(): Promise<TraceListResponse> {
  return request<TraceListResponse>(`${GATEWAY_BASE}/v1/traces`);
}

export async function getTrace(id: string): Promise<Trace> {
  return request<Trace>(`${GATEWAY_BASE}/v1/traces/${encodeURIComponent(id)}`);
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

// ---------- Identity APIs ----------

export async function login(
  tenantID: string,
  username: string,
  password: string,
): Promise<LoginResponse> {
  return request<LoginResponse>(`${GATEWAY_BASE}/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant_id: tenantID, username, password }),
  });
}

export async function register(
  tenantID: string,
  username: string,
  password: string,
  email?: string,
): Promise<{ user: AuthUser }> {
  return request<{ user: AuthUser }>(`${GATEWAY_BASE}/v1/auth/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant_id: tenantID, username, password, email }),
  });
}

export async function refreshAuth(): Promise<LoginResponse> {
  return request<LoginResponse>(`${GATEWAY_BASE}/v1/auth/refresh`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ refresh_token: getRefreshToken() }),
  });
}

export async function logout(): Promise<void> {
  await request<{ ok: boolean }>(`${GATEWAY_BASE}/v1/auth/logout`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ refresh_token: getRefreshToken() }),
  }).catch(() => undefined); // stateless: always clear locally
  clearAuth();
}

export async function listUsers(query = "", limit = 50, offset = 0): Promise<UserListResponse> {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (query) params.set("query", query);
  return request<UserListResponse>(`${GATEWAY_BASE}/v1/users?${params.toString()}`);
}

export interface CreateUserInput {
  username: string;
  password: string;
  email?: string;
  roles?: string[];
  departments?: string[];
}

export async function createUser(input: CreateUserInput): Promise<{ user: AuthUser }> {
  return request<{ user: AuthUser }>(`${GATEWAY_BASE}/v1/users`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export interface UpdateUserInput {
  email?: string;
  roles?: string[];
  departments?: string[];
  status?: string;
  password?: string;
}

export async function updateUser(id: string, patch: UpdateUserInput): Promise<{ user: AuthUser }> {
  return request<{ user: AuthUser }>(`${GATEWAY_BASE}/v1/users/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
}

export async function deleteUser(id: string): Promise<{ success: boolean }> {
  return request<{ success: boolean }>(`${GATEWAY_BASE}/v1/users/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export async function listRoles(): Promise<{ roles: RoleInfo[] }> {
  return request<{ roles: RoleInfo[] }>(`${GATEWAY_BASE}/v1/roles`);
}

export async function upsertRole(input: {
  name: string;
  permissions: string[];
  description?: string;
}): Promise<{ role: RoleInfo }> {
  return request<{ role: RoleInfo }>(`${GATEWAY_BASE}/v1/roles`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function deleteRole(name: string): Promise<{ success: boolean }> {
  return request<{ success: boolean }>(`${GATEWAY_BASE}/v1/roles/${encodeURIComponent(name)}`, {
    method: "DELETE",
  });
}

export async function listPermissions(): Promise<{ permissions: PermissionInfo[] }> {
  return request<{ permissions: PermissionInfo[] }>(`${GATEWAY_BASE}/v1/permissions`);
}

// ---------- Usage & quota ----------

export async function getUsage(): Promise<UsageResponse> {
  return request<UsageResponse>(`${GATEWAY_BASE}/v1/usage`);
}

// ---------- Knowledge base APIs ----------
export async function listKnowledgeBases(): Promise<{ knowledge_bases: KnowledgeBase[] }> {
  return request<{ knowledge_bases: KnowledgeBase[] }>(`${GATEWAY_BASE}/v1/knowledge-bases`);
}

export async function createKnowledgeBase(input: {
  name: string;
  description?: string;
}): Promise<{ knowledge_base: KnowledgeBase }> {
  return request<{ knowledge_base: KnowledgeBase }>(`${GATEWAY_BASE}/v1/knowledge-bases`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function updateKnowledgeBase(
  id: string,
  input: { name?: string; description?: string },
): Promise<{ knowledge_base: KnowledgeBase }> {
  return request<{ knowledge_base: KnowledgeBase }>(
    `${GATEWAY_BASE}/v1/knowledge-bases/${encodeURIComponent(id)}`,
    {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    },
  );
}

export async function deleteKnowledgeBase(id: string): Promise<{ success: boolean }> {
  return request<{ success: boolean }>(
    `${GATEWAY_BASE}/v1/knowledge-bases/${encodeURIComponent(id)}`,
    { method: "DELETE" },
  );
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
