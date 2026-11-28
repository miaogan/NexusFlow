import { useCallback, useEffect, useState } from "react";
import { hasPermission, listAudit } from "../api/client";
import type { AuditEvent } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner, Tag } from "../components/ui";

const inputClass =
  "w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40";

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function decisionClass(decision: string): string {
  const value = decision.toLowerCase();
  if (value.includes("deny") || value.includes("reject")) {
    return "bg-rose-500/10 text-rose-300";
  }
  if (value.includes("mask")) {
    return "bg-amber-500/10 text-amber-300";
  }
  return "bg-emerald-500/10 text-emerald-300";
}

function toRFC3339(value: string): string {
  if (!value) return "";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? "" : parsed.toISOString();
}

export default function Audit() {
  const canRead = hasPermission("trace:read");
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filters, setFilters] = useState({
    action: "",
    decision: "",
    since: "",
    until: "",
  });

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await listAudit({
        limit: 100,
        action: filters.action,
        decision: filters.decision,
        since: toRFC3339(filters.since),
        until: toRFC3339(filters.until),
      });
      setEvents(res.events ?? []);
    } catch (e) {
      setError(errMsg(e));
      setEvents([]);
    } finally {
      setLoading(false);
    }
  }, [filters]);

  useEffect(() => {
    if (canRead) void load();
  }, [canRead]);

  if (!canRead) {
    return (
      <div>
        <PageHeader title="审计日志" description="工具调用、审批决策与 PII 脱敏记录" />
        <Card>
          <EmptyState title="无权访问" hint="当前账号缺少 trace:read 权限。" />
        </Card>
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="审计日志"
        description="工具调用、审批决策与 PII 脱敏记录，按租户隔离"
        actions={
          <Button variant="ghost" onClick={() => load()} disabled={loading}>
            刷新
          </Button>
        }
      />

      {error && <div className="panel mb-4 p-3 text-xs text-rose-300">{error}</div>}

      <Card title="筛选">
        <div className="grid gap-2 md:grid-cols-2 lg:grid-cols-4">
          <input
            value={filters.action}
            onChange={(e) => setFilters({ ...filters, action: e.target.value })}
            placeholder="动作，如 tool.call"
            className={inputClass}
          />
          <input
            value={filters.decision}
            onChange={(e) => setFilters({ ...filters, decision: e.target.value })}
            placeholder="决策，如 allowed / denied"
            className={inputClass}
          />
          <input
            type="datetime-local"
            value={filters.since}
            onChange={(e) => setFilters({ ...filters, since: e.target.value })}
            className={inputClass}
          />
          <input
            type="datetime-local"
            value={filters.until}
            onChange={(e) => setFilters({ ...filters, until: e.target.value })}
            className={inputClass}
          />
        </div>
        <div className="mt-2 flex gap-2">
          <Button onClick={() => load()} disabled={loading}>
            {loading ? "查询中…" : "查询"}
          </Button>
          <Button
            variant="ghost"
            onClick={() => setFilters({ action: "", decision: "", since: "", until: "" })}
            disabled={loading}
          >
            重置
          </Button>
        </div>
      </Card>

      <div className="mt-4">
        <Card title={`记录 · ${events.length}`}>
          {loading && events.length === 0 ? (
            <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
              <Spinner /> 加载中…
            </div>
          ) : events.length === 0 ? (
            <EmptyState title="暂无审计记录" hint="工具调用与审批决策发生后会出现在这里。" />
          ) : (
            <ul className="space-y-1.5">
              {events.map((event) => (
                <li key={event.id} className="rounded border border-white/5 px-2 py-1.5">
                  <div className="flex flex-wrap items-center gap-2 text-[10px]">
                    <span className={`rounded px-1.5 py-0.5 ${decisionClass(event.decision)}`}>
                      {event.decision || "allowed"}
                    </span>
                    <span className="text-slate-300">{event.action}</span>
                    <Tag>{event.risk}</Tag>
                    <span className="text-slate-500">status {event.status}</span>
                    {event.resource && <span className="text-slate-500">{event.resource}</span>}
                    <span className="ml-auto text-slate-600">
                      {new Date(event.ts * 1000).toLocaleString()}
                    </span>
                  </div>
                  <div className="mt-1 flex flex-wrap gap-3 text-[10px] text-slate-500">
                    <span>user {event.user_id || "-"}</span>
                    <span>tenant {event.tenant_id || "-"}</span>
                    {event.trace_id && <span>trace {event.trace_id}</span>}
                  </div>
                  {Object.keys(event.detail ?? {}).length > 0 && (
                    <details className="mt-1">
                      <summary className="cursor-pointer text-[10px] text-slate-500">查看详情</summary>
                      <pre className="mt-1 max-h-40 overflow-auto rounded bg-ink-950 p-2 text-[10px] text-slate-400">
                        {JSON.stringify(event.detail, null, 2)}
                      </pre>
                    </details>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
