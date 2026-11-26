import type { Dag, SearchResponse, ValidationResult } from "../types";

const GATEWAY_BASE = import.meta.env.VITE_GATEWAY_URL ?? "/api";
const WORKFLOW_BASE = import.meta.env.VITE_WORKFLOW_URL ?? "/wf";

async function parse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const detail = await response.text();
    throw new Error(detail || `HTTP ${response.status}`);
  }
  return (await response.json()) as T;
}

export async function runSearch(query: string): Promise<SearchResponse> {
  const response = await fetch(`${GATEWAY_BASE}/v1/search`, {
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
  return parse<SearchResponse>(response);
}

export async function validateDag(dag: Dag): Promise<ValidationResult> {
  const response = await fetch(`${WORKFLOW_BASE}/v1/workflows/validate`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dag }),
  });
  return parse<ValidationResult>(response);
}

export async function runDag(dag: Dag): Promise<unknown> {
  const response = await fetch(`${WORKFLOW_BASE}/v1/workflows/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dag, input: { source: "web-canvas" } }),
  });
  return parse<unknown>(response);
}
