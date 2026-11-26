import { useState } from "react";
import { runSearch } from "../api/client";
import type { SearchResponse } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner } from "../components/ui";

export default function Search() {
  const [query, setQuery] = useState("");
  const [result, setResult] = useState<SearchResponse | null>(null);
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
          <Card
            title={`召回 ${result.recall_count} 路 · ${result.chunks.length} 个片段`}
            action={
              <div className="flex items-center gap-2 text-[10px] text-slate-500">
                <span>trace: <span className="font-mono text-slate-400">{result.trace_id.slice(0, 8)}</span></span>
                <span className={result.rerank_applied ? "text-accent" : ""}>{result.rerank_applied ? "已重排" : "未重排"}</span>
              </div>
            }
          >
            {result.chunks.length === 0 ? (
              <EmptyState title="无命中结果" hint="尝试调整查询或确认索引已构建。" />
            ) : (
              <ul className="space-y-3">
                {result.chunks.map((chunk, idx) => (
                  <li key={chunk.chunk_id} className="rounded-lg border border-white/5 bg-white/[0.02] p-3">
                    <div className="mb-1.5 flex items-center justify-between">
                      <span className="text-[10px] font-mono text-slate-500">#{idx + 1} {chunk.chunk_id}</span>
                      <span className="text-[10px] text-accent">score {chunk.score.toFixed(4)}</span>
                    </div>
                    <p className="text-xs leading-relaxed text-slate-300">{chunk.content}</p>
                    {chunk.meta && Object.keys(chunk.meta).length > 0 && (
                      <div className="mt-2 flex flex-wrap gap-1">
                        {Object.entries(chunk.meta).map(([k, v]) => (
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
        )}
      </div>
    </div>
  );
}
