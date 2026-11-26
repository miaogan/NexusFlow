import { useEffect, useState } from "react";
import { getTrace, runSearch, submitFeedback } from "../api/client";
import type { SearchResponse, Trace, TraceSpan } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner } from "../components/ui";

function durationMs(s: TraceSpan): number {
  return new Date(s.end_time).getTime() - new Date(s.start_time).getTime();
}

function statusStyle(status: string): string {
  if (status === "error") return "text-rose-400";
  if (status === "ok") return "text-emerald-400";
  return "text-slate-500";
}

function scalarEntries(obj: unknown): [string, string][] {
  if (!obj || typeof obj !== "object") return [];
  return Object.entries(obj as Record<string, unknown>)
    .filter(([, v]) => ["string", "number", "boolean"].includes(typeof v))
    .slice(0, 6)
    .map(([k, v]) => [k, String(v)]);
}

export default function Search() {
  const [query, setQuery] = useState("");
  const [result, setResult] = useState<SearchResponse | null>(null);
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // feedback / trace-breakdown state, scoped to the current result.trace_id
  const [rating, setRating] = useState<number | null>(null);
  const [submittingRating, setSubmittingRating] = useState(false);
  const [showTrace, setShowTrace] = useState(false);
  const [trace, setTrace] = useState<Trace | null>(null);
  const [loadingTrace, setLoadingTrace] = useState(false);
  const [traceError, setTraceError] = useState<string | null>(null);

  // Reset feedback + trace view whenever a new search result (new trace_id) arrives.
  useEffect(() => {
    setRating(null);
    setTrace(null);
    setTraceError(null);
    setShowTrace(false);
  }, [result?.trace_id]);

  const handleSearch = async () => {
    if (!query.trim()) return;
    setSearching(true);
    setError(null);
    try {
      setResult(await runSearch(query.trim()));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setResult(null);
    } finally {
      setSearching(false);
    }
  };

  const handleRating = async (value: number) => {
    if (!result || submittingRating) return;
    if (rating === value) {
      setRating(null); // toggle off locally; backend keeps the last score
      return;
    }
    const prev = rating;
    setRating(value);
    setSubmittingRating(true);
    try {
      await submitFeedback(result.trace_id, value);
    } catch (e) {
      setRating(prev);
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmittingRating(false);
    }
  };

  const openTrace = async () => {
    if (!result) return;
    if (showTrace) {
      setShowTrace(false);
      return;
    }
    setShowTrace(true);
    if (trace?.id === result.trace_id) return;
    setLoadingTrace(true);
    setTraceError(null);
    try {
      setTrace(await getTrace(result.trace_id));
    } catch (e) {
      setTraceError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingTrace(false);
    }
  };

  const renderTree = () => {
    if (!trace) return null;
    const byParent = new Map<string, TraceSpan[]>();
    for (const s of trace.spans ?? []) {
      const pid = s.parent_id ?? "";
      if (!byParent.has(pid)) byParent.set(pid, []);
      byParent.get(pid)!.push(s);
    }
    for (const arr of byParent.values()) {
      arr.sort((a, b) => +new Date(a.start_time) - +new Date(b.start_time));
    }
    const renderNode = (span: TraceSpan, depth: number) => {
      const kids = byParent.get(span.span_id) ?? [];
      const metrics = [...scalarEntries(span.output), ...scalarEntries(span.attributes)];
      return (
        <div key={span.span_id} style={{ paddingLeft: depth * 18 }} className="border-l border-white/5 py-1.5 pl-3">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-mono text-xs text-slate-200">{span.name}</span>
            <span className="text-[10px] text-slate-500">{durationMs(span).toFixed(1)} ms</span>
            <span className={`text-[10px] uppercase ${statusStyle(span.status)}`}>{span.status}</span>
            {metrics.map(([k, v]) => (
              <span key={k} className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                {k}: <span className="text-slate-300">{v}</span>
              </span>
            ))}
          </div>
          {span.status_msg && <div className="mt-0.5 text-[10px] text-rose-300/80">{span.status_msg}</div>}
          {kids.map((k) => renderNode(k, depth + 1))}
        </div>
      );
    };
    if (!trace.root) {
      return <div className="text-xs text-slate-500">该 trace 无 root span。</div>;
    }
    return renderNode(trace.root, 0);
  };

  const scores = trace?.scores ?? [];

  return (
    <div>
      <PageHeader title="检索测试" description="向量 + BM25 多路召回，RRF 融合，可选 Cross-Encoder 重排" />

      <Card>
        <div className="flex gap-2">
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && handleSearch()}
            placeholder="输入查询，例如：向量召回与 RRF 融合"
            className="flex-1 rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-sm text-slate-200 outline-none focus:border-accent/40"
          />
          <Button onClick={handleSearch} disabled={searching || !query.trim()}>
            {searching ? <Spinner /> : "检索"}
          </Button>
        </div>
      </Card>

      <div className="mt-4">
        {error && (
          <div className="panel p-4 text-xs text-rose-300">检索失败：{error}</div>
        )}
        {result && (
          <>
            <Card
              title={`召回 ${result.recall_count} 路 · ${result.chunks.length} 个片段`}
              action={
                <div className="flex items-center gap-3 text-[10px] text-slate-500">
                  <span>
                    trace: <span className="font-mono text-slate-400">{result.trace_id.slice(0, 8)}</span>
                  </span>
                  <span className={result.rerank_applied ? "text-accent" : ""}>
                    {result.rerank_applied ? "已重排" : "未重排"}
                  </span>
                  <div className="flex items-center gap-1">
                    <button
                      type="button"
                      onClick={() => handleRating(1)}
                      disabled={submittingRating}
                      title="赞"
                      className={`rounded px-1.5 py-0.5 text-xs transition ${
                        rating === 1 ? "bg-accent/20 text-accent" : "text-slate-500 hover:text-slate-300"
                      }`}
                    >
                      👍
                    </button>
                    <button
                      type="button"
                      onClick={() => handleRating(-1)}
                      disabled={submittingRating}
                      title="踩"
                      className={`rounded px-1.5 py-0.5 text-xs transition ${
                        rating === -1 ? "bg-rose-500/20 text-rose-400" : "text-slate-500 hover:text-slate-300"
                      }`}
                    >
                      👎
                    </button>
                  </div>
                  <button
                    type="button"
                    onClick={openTrace}
                    className={`rounded px-2 py-0.5 transition ${
                      showTrace ? "bg-accent/20 text-accent" : "text-slate-400 hover:text-slate-200"
                    }`}
                  >
                    链路分解 {showTrace ? "▲" : "▼"}
                  </button>
                </div>
              }
            >
              {result.chunks.length === 0 ? (
                <EmptyState title="无命中结果" hint="尝试调整查询或确认索引已构建。" />
              ) : (
                <ul className="space-y-3">
                  {result.chunks.map((chunk, idx) => (
                    <li key={chunk.chunk_id} className="rounded-lg border border-white/5 bg-white/[0.02] p-3">
                      <div className="mb-1.5 flex items-center justify-between gap-2">
                        <span className="truncate text-[10px] font-mono text-slate-500">
                          #{idx + 1} {chunk.chunk_id}
                        </span>
                        <div className="flex shrink-0 items-center gap-1.5">
                          {chunk.meta?.file_name && (
                            <span
                              className="rounded bg-accent/10 px-1.5 py-0.5 text-[9px] text-accent"
                              title="来源文件"
                            >
                              📄 {chunk.meta.file_name}
                            </span>
                          )}
                          {chunk.meta?.source && (
                            <span className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                              {chunk.meta.source}
                            </span>
                          )}
                          <span className="text-[10px] text-accent">score {chunk.score.toFixed(4)}</span>
                        </div>
                      </div>
                      <p className="text-xs leading-relaxed text-slate-300">{chunk.content}</p>
                      {chunk.meta &&
                        Object.keys(chunk.meta).filter((k) => k !== "file_name" && k !== "source").length > 0 && (
                          <div className="mt-2 flex flex-wrap gap-1">
                            {Object.entries(chunk.meta)
                              .filter(([k]) => k !== "file_name" && k !== "source")
                              .map(([k, v]) => (
                                <span key={k} className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-500">
                                  {k}: {v}
                                </span>
                              ))}
                          </div>
                        )}
                    </li>
                  ))}
                </ul>
              )}
            </Card>

            {showTrace && (
              <Card title="链路分解" className="mt-4">
                {loadingTrace && (
                  <div className="flex items-center gap-2 py-4 text-xs text-slate-500">
                    <Spinner /> 加载 trace…
                  </div>
                )}
                {!loadingTrace && traceError && (
                  <div className="py-2 text-xs text-rose-300">加载 trace 失败：{traceError}</div>
                )}
                {!loadingTrace && !traceError && trace && (
                  <>
                    <div className="mb-2 text-[10px] text-slate-500">
                      trace_id: <span className="font-mono text-slate-400">{trace.id}</span>
                      {scores.length > 0 && (
                        <span className="ml-3">
                          反馈 {scores.length} 条：
                          {scores.map((s, i) => (
                            <span key={i} className="ml-1 rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                              {s.name}={s.value}
                              {s.comment ? ` “${s.comment}”` : ""}
                            </span>
                          ))}
                        </span>
                      )}
                    </div>
                    <div className="rounded-lg border border-white/5 bg-white/[0.02] p-2">{renderTree()}</div>
                  </>
                )}
              </Card>
            )}
          </>
        )}
      </div>
    </div>
  );
}
