import { useState } from "react";
import type { PipelineNode, SearchResponse, ValidationResult } from "../types";

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
}: Props) {
  const [query, setQuery] = useState("三路召回与重排如何工作？");

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
            </div>
          ) : (
            <p className="text-[11px] text-slate-500">在画布中点击节点查看详情。</p>
          )}
        </section>
      </div>
    </aside>
  );
}
