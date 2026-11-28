import { useEffect, useState } from "react";
import type { PipelineNode, SearchResponse, ValidationResult, WorkflowRunState } from "../types";
import { Spinner, StatePill } from "./ui";

const CONFIG_SAMPLES: Record<string, Record<string, unknown>> = {
  set: { key: "name", value: "value" },
  query: { query: "NexusFlow 的检索链路如何工作？", kb_id: "default", top_k: 20, final_top_n: 5 },
  sandbox: {
    code: "import json\nprint(json.dumps({'ok': True}))",
    runtime: "python",
    timeout_seconds: 10,
    packages: [],
  },
  condition: { field: "intent", equals: "knowledge_qa", then: ["query"], else: ["noop"] },
  loop: {
    items: "items",
    max_concurrency: 2,
    sub_dag: { name: "sub", nodes: [{ id: "noop", type: "noop" }], edges: [] },
  },
  human_approval: { prompt: "请确认发布" },
  agent: { capability: "intent.classify", input: { message: "hello" } },
  nl2sql: { sql: "SELECT 1" },
  noop: {},
};

const CONFIG_HINTS: Record<string, string> = {
  set: "key / value",
  query: "query / query_from、kb_id、top_k、final_top_n、rerank_model",
  sandbox: "code、runtime、timeout_seconds、packages、input",
  condition: "field、equals/contains/in、then、else",
  loop: "items、max_concurrency、sub_dag",
  human_approval: "prompt；需 HITL 开启",
  agent: "agent 或 capability、input；需 Agent 节点开启",
  nl2sql: "sql 或 sql_from",
};

const DECLARATIVE_TYPES = new Set(["ingest", "parse", "index"]);
const REAL_TYPES = new Set(["query", "sandbox", "agent", "nl2sql"]);

interface Props {
  selected: PipelineNode | null;
  search: SearchResponse | null;
  searching: boolean;
  searchError: string | null;
  onSearch: (query: string) => void;
  validation: ValidationResult | null;
  validationError: string | null;
  onValidate: () => void;
  onRun: () => void;
  busy: boolean;
  runOutput: string | null;
  runs?: WorkflowRunState[];
  runsLoading?: boolean;
  readOnly?: boolean;
  onApplyConfig?: (nodeId: string, config: Record<string, unknown> | undefined) => void;
}

export default function Inspector({
  selected,
  search,
  searching,
  searchError,
  onSearch,
  validation,
  validationError,
  onValidate,
  onRun,
  busy,
  runOutput,
  runs = [],
  runsLoading = false,
  readOnly = false,
  onApplyConfig,
}: Props) {
  const [query, setQuery] = useState("三路召回与重排如何工作？");
  const [configText, setConfigText] = useState("");
  const [configError, setConfigError] = useState<string | null>(null);

  useEffect(() => {
    if (!selected) {
      setConfigText("");
      setConfigError(null);
      return;
    }
    const initial = selected.data.config ?? CONFIG_SAMPLES[selected.data.type] ?? {};
    setConfigText(JSON.stringify(initial, null, 2));
    setConfigError(null);
  }, [selected?.id]);

  const applyConfig = () => {
    if (!selected || !onApplyConfig || readOnly) return;
    const text = configText.trim();
    if (!text) {
      onApplyConfig(selected.id, undefined);
      setConfigError(null);
      return;
    }
    try {
      const parsed = JSON.parse(text) as unknown;
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
        throw new Error("配置必须是 JSON 对象");
      }
      onApplyConfig(selected.id, parsed as Record<string, unknown>);
      setConfigError(null);
    } catch (error) {
      setConfigError(error instanceof Error ? error.message : String(error));
    }
  };

  return (
    <aside className="panel flex h-full flex-col overflow-hidden">
      <div className="border-b border-white/5 px-4 py-3">
        <p className="panel-title">运行面板</p>
      </div>

      <div className="flex-1 space-y-5 overflow-y-auto p-4">
        <section>
          <p className="mb-2 text-xs font-semibold text-slate-300">检索调试</p>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (query.trim()) onSearch(query.trim());
            }}
            className="space-y-2"
          >
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="输入查询…"
              className="w-full rounded-lg border border-white/10 bg-ink-800/80 px-3 py-2 text-xs text-slate-200 outline-none transition focus:border-accent/50"
            />
            <button
              type="submit"
              disabled={searching}
              className="w-full rounded-lg bg-accent px-3 py-2 text-xs font-semibold text-ink-950 transition hover:bg-accent-dim disabled:opacity-50"
            >
              {searching ? "检索中…" : "运行检索"}
            </button>
          </form>

          {searchError && <p className="mt-2 text-[11px] text-rose-400">{searchError}</p>}

          {search && (
            <div className="mt-3 space-y-2">
              <p className="text-[10px] text-slate-500">
                召回 {search.recall_count} 条 · Rerank {search.rerank_applied ? "已启用" : "未启用"}
              </p>
              {search.chunks.map((chunk) => (
                <div key={chunk.chunk_id} className="rounded-lg border border-white/5 bg-ink-800/50 p-2.5">
                  <div className="flex items-center justify-between text-[10px] text-slate-500">
                    <span className="truncate">{chunk.chunk_id}</span>
                    <span className="text-accent">{chunk.score.toFixed(4)}</span>
                  </div>
                  <p className="mt-1 line-clamp-3 text-[11px] leading-relaxed text-slate-300">
                    {chunk.content}
                  </p>
                </div>
              ))}
            </div>
          )}
        </section>

        <section>
          <p className="mb-2 text-xs font-semibold text-slate-300">DAG 校验</p>
          <div className="flex gap-2">
            <button
              type="button"
              onClick={onValidate}
              disabled={busy}
              className="flex-1 rounded-lg border border-white/10 bg-ink-800/60 px-3 py-2 text-[11px] text-slate-200 transition hover:border-accent/40 disabled:opacity-50"
            >
              校验
            </button>
            <button
              type="button"
              onClick={onRun}
              disabled={busy}
              className="flex-1 rounded-lg border border-white/10 bg-ink-800/60 px-3 py-2 text-[11px] text-slate-200 transition hover:border-accent/40 disabled:opacity-50"
            >
              试运行
            </button>
          </div>
          {validation && (
            <p className="mt-2 text-[11px] text-emerald-400">
              校验通过 · 执行顺序 {validation.order.join(" → ")}
            </p>
          )}
          {validationError && <p className="mt-2 text-[11px] text-rose-400">{validationError}</p>}
          {runOutput && (
            <pre className="mt-2 max-h-40 overflow-auto rounded-lg border border-white/5 bg-ink-950/70 p-2 text-[10px] leading-relaxed text-slate-400">
              {runOutput}
            </pre>
          )}
        </section>

        <section>
          <p className="mb-2 text-xs font-semibold text-slate-300">节点详情</p>
          {selected ? (
            <div className="space-y-2 rounded-lg border border-white/5 bg-ink-800/50 p-3">
              <div className="flex items-center justify-between">
                <span className="text-xs font-semibold text-slate-200">{selected.data.label}</span>
                <span className="rounded bg-accent/10 px-1.5 py-0.5 text-[10px] text-accent">
                  {selected.data.stage}
                </span>
              </div>
              <p className="text-[11px] text-slate-500">{selected.data.caption}</p>
              <p
                className={`text-[10px] ${
                  DECLARATIVE_TYPES.has(selected.data.type)
                    ? "text-amber-300"
                    : REAL_TYPES.has(selected.data.type)
                      ? "text-emerald-300"
                      : "text-slate-500"
                }`}
              >
                {DECLARATIVE_TYPES.has(selected.data.type)
                  ? "声明式阶段 · 不实际执行"
                  : REAL_TYPES.has(selected.data.type)
                    ? "真实执行节点"
                    : "编排节点"}
              </p>
              <dl className="space-y-1 text-[10px] text-slate-500">
                <div className="flex justify-between">
                  <dt>node id</dt>
                  <dd className="font-mono text-slate-400">{selected.id}</dd>
                </div>
                <div className="flex justify-between">
                  <dt>type</dt>
                  <dd className="font-mono text-slate-400">{selected.data.type}</dd>
                </div>
              </dl>

              <div className="border-t border-white/5 pt-2">
                <div className="mb-1 flex items-center justify-between">
                  <span className="text-[10px] text-slate-500">节点配置（JSON）</span>
                  <button
                    type="button"
                    disabled={readOnly}
                    onClick={() =>
                      setConfigText(
                        JSON.stringify(CONFIG_SAMPLES[selected.data.type] ?? {}, null, 2),
                      )
                    }
                    className="text-[9px] text-slate-500 transition hover:text-slate-300 disabled:opacity-40"
                  >
                    填入示例
                  </button>
                </div>
                <textarea
                  value={configText}
                  onChange={(event) => setConfigText(event.target.value)}
                  rows={8}
                  spellCheck={false}
                  readOnly={readOnly}
                  className="w-full resize-y rounded-lg border border-white/10 bg-ink-950 p-2 font-mono text-[10px] leading-relaxed text-slate-300 outline-none focus:border-accent/40 disabled:opacity-60"
                />
                {configError && <p className="mt-1 text-[10px] text-rose-300">{configError}</p>}
                <div className="mt-1 flex items-center gap-2">
                  <button
                    type="button"
                    onClick={applyConfig}
                    disabled={readOnly || !onApplyConfig}
                    className="rounded border border-accent/40 bg-accent/10 px-2 py-0.5 text-[10px] text-accent transition hover:bg-accent/15 disabled:opacity-40"
                  >
                    应用配置
                  </button>
                  <span className="text-[9px] text-slate-600">
                    {CONFIG_HINTS[selected.data.type] ?? ""}
                  </span>
                </div>
              </div>
            </div>
          ) : (
            <p className="text-[11px] text-slate-500">在画布中点击节点查看详情。</p>
          )}
        </section>

        <section>
          <div className="mb-2 flex items-center justify-between">
            <p className="text-xs font-semibold text-slate-300">运行历史</p>
            {runsLoading && <Spinner className="h-3 w-3" />}
          </div>
          {runs.length === 0 ? (
            <p className="text-[11px] text-slate-500">当前工作流还没有运行记录。</p>
          ) : (
            <ul className="space-y-1.5">
              {runs.slice(0, 6).map((run) => (
                <li
                  key={run.id}
                  className="rounded-lg border border-white/5 bg-ink-800/50 px-2.5 py-2"
                >
                  <div className="flex items-center gap-2">
                    <StatePill state={run.status} />
                    <span className="ml-auto text-[9px] text-slate-600">
                      {run.created_at?.replace("T", " ").slice(5, 16)}
                    </span>
                  </div>
                  <p className="mt-1 truncate text-[9px] text-slate-500">
                    {run.order?.length ?? 0} 节点 · {run.results?.length ?? 0} 完成
                    {run.error ? ` · ${run.error}` : ""}
                  </p>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </aside>
  );
}
