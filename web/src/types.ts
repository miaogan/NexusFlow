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

export type { Edge };
