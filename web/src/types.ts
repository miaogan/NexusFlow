import type { Edge, Node } from "reactflow";

export type Stage = "ingest" | "parse" | "index" | "sandbox" | "query";

export interface PipelineNodeData {
  label: string;
  caption: string;
  stage: Stage;
  type: string;
  config?: Record<string, unknown>;
}

export type PipelineNode = Node<PipelineNodeData>;

export interface DagNode {
  id: string;
  type: string;
  config?: Record<string, unknown>;
}

export interface DagEdge {
  from: string;
  to: string;
}

export interface Dag {
  name: string;
  nodes: DagNode[];
  edges: DagEdge[];
}

export interface PaletteItem {
  stage: Stage;
  type: string;
  label: string;
  caption: string;
  description: string;
}

export interface SearchChunk {
  chunk_id: string;
  content: string;
  score: number;
  meta?: Record<string, string>;
}

export interface SearchResponse {
  chunks: SearchChunk[];
  trace_id: string;
  recall_count: number;
  rerank_applied: boolean;
}

export interface ValidationResult {
  valid: boolean;
  order: string[];
}

export interface TaskSummary {
  task_id: string;
  status: string;
  progress: number;
  file_name: string;
  file_type: string;
  file_size: number;
  content_hash?: string;
  error_msg?: string;
  kb_id?: string;
  created_at?: string;
  updated_at?: string;
}

export interface TaskListResponse {
  tasks: TaskSummary[];
  total: number;
}

export interface TaskDetail {
  task_id: string;
  status: string;
  progress: number;
  error_msg?: string;
  content_hash?: string;
  metadata?: Record<string, string>;
  file_name?: string;
  file_type?: string;
  file_size?: number;
  kb_id?: string;
  created_at?: string;
  updated_at?: string;
}

export interface UploadResult {
  task_id: string;
  status: string;
  message: string;
  content_hash?: string;
  object_key?: string;
}

export interface SandboxExecuteRequest {
  code: string;
  runtime?: "python" | "node";
  packages?: string[];
  env_vars?: Record<string, string>;
  input_json?: string;
  timeout_seconds?: number;
}

export interface SandboxResult {
  success: boolean;
  output_json?: string;
  stdout?: string;
  stderr?: string;
  sandbox_id?: string;
  error_msg?: string;
}

export interface MeInfo {
  tenant_id: string;
  user_id: string;
  roles: string[];
  departments?: string[];
  username?: string;
  email?: string;
  status?: string;
}

// ---------- Observability / tracing ----------

export interface TraceSpan {
  trace_id: string;
  span_id: string;
  parent_id?: string;
  name: string;
  start_time: string;
  end_time: string;
  status: string;
  status_msg?: string;
  input?: unknown;
  output?: unknown;
  attributes?: Record<string, unknown>;
}

export interface TraceScore {
  trace_id: string;
  name: string;
  value: number;
  comment?: string;
  ts: string;
}

export interface Trace {
  id: string;
  root: TraceSpan | null;
  spans: TraceSpan[];
  scores: TraceScore[];
}

export interface TraceListResponse {
  traces: Trace[];
  enabled: boolean;
}

export interface FeedbackRequest {
  trace_id: string;
  value: number;
  comment?: string;
  name?: string;
}

// ---------- RAG chat ----------

export interface Citation {
  chunk_id: string;
  content: string;
  score: number;
  meta?: Record<string, string>;
}

export interface IntentInfo {
  intent: "knowledge_qa" | "chitchat" | "clarify" | string;
  confidence: number;
  need_retrieval: boolean;
  reason?: string;
  source?: string;
}

export interface ChatResponse {
  conversation_id: string;
  message_id: string;
  answer: string;
  citations: Citation[];
  trace_id?: string;
  created_at: string;
}

export interface ConversationSummary {
  id: string;
  title: string;
  message_count: number;
  created_at: string;
  updated_at: string;
}

export interface ConversationListResponse {
  conversations: ConversationSummary[];
  total: number;
  limit: number;
  offset: number;
}

export interface ChatMessage {
  id: string;
  role: "user" | "assistant" | string;
  content: string;
  citations?: Citation[];
  trace_id?: string;
  intent?: string;
  query?: string;
  created_at: string;
}

export interface ConversationDetail {
  conversation: ConversationSummary;
  messages: ChatMessage[];
}

// ---------- Identity / auth ----------

export interface AuthUser {
  id: string;
  tenant_id: string;
  username: string;
  email: string;
  roles: string[];
  departments: string[];
  status: string;
  created_at: string;
  updated_at: string;
}

export interface LoginResponse {
  access_token: string;
  refresh_token: string;
  expires_in: number;
  user: AuthUser;
}

export interface RoleInfo {
  name: string;
  permissions: string[];
  builtin: boolean;
  description: string;
}

export interface PermissionInfo {
  key: string;
  description: string;
}

export interface UserListResponse {
  users: AuthUser[];
  total: number;
}

export interface KnowledgeBase {
  id: string;
  tenant_id: string;
  name: string;
  description: string;
  role?: "kb:admin" | "kb:editor" | "kb:viewer" | string;
  status?: string;
  created_by?: string;
  embedding_model?: string;
  created_at?: string;
  updated_at?: string;
}

export interface KnowledgeBasePermission {
  id: string;
  tenant_id: string;
  kb_id: string;
  subject_type: "user" | "role" | string;
  subject_id: string;
  permission: "kb:admin" | "kb:editor" | "kb:viewer" | string;
  created_at?: string;
}

export const KB_ROLE_LABELS: Record<string, string> = {
  "kb:admin": "管理员",
  "kb:editor": "编辑者",
  "kb:viewer": "只读",
};

// ---------- Usage / quota ----------

export interface KBUsage {
  kb_id: string;
  storage_bytes: number;
  file_count: number;
}

export interface TenantUsage {
  tenant_id: string;
  storage_bytes: number;
  file_count: number;
  query_count: number;
  knowledge_bases: KBUsage[];
}

export interface UsageQuota {
  max_storage_bytes: number;
  max_files: number;
  max_queries: number;
}

export interface UsageResponse {
  usage: TenantUsage;
  quota: UsageQuota;
  remaining: UsageQuota;
}

export interface JwtClaims {
  tenant_id?: string;
  user_id?: string;
  roles?: string[];
  departments?: string[];
  permissions?: string[];
  exp?: number;
}

// ---------- Custom SubAgents / skills ----------

export interface SubAgentSandboxSummary {
  runtime: string;
  timeout_seconds: number;
  packages: string[];
  code_bytes: number;
  configured: boolean;
}

export interface SubAgentRecord {
  name: string;
  tenant_id: string;
  kind: "http" | "sandbox";
  capabilities: string[];
  endpoint: string;
  priority: number;
  healthy: boolean;
  failures?: number;
  ttl_seconds: number;
  version: string;
  risk: string;
  model: string;
  tools: string[];
  skills: string[];
  sandbox?: SubAgentSandboxSummary;
  permissions?: string[];
  input_schema?: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
  expires_in?: number | null;
}

export interface SubAgentSandboxInput {
  code?: string;
  runtime?: string;
  timeout_seconds?: number;
  packages?: string[];
}

export interface SubAgentInput {
  name: string;
  kind: "http" | "sandbox";
  capabilities: string[];
  endpoint?: string;
  skills?: string[];
  priority?: number;
  timeout_ms?: number;
  risk?: string;
  sandbox?: SubAgentSandboxInput;
}

export interface SkillInfo {
  skill: string;
  agent: string;
  tenant_id: string;
  capabilities: string[];
  priority: number;
}

export interface SubAgentCallResult {
  ok: boolean;
  agent?: string;
  data?: Record<string, unknown>;
  error?: string;
  latency_ms?: number;
  attempts?: number;
}

export type { Edge };
