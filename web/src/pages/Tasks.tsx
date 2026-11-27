import { useCallback, useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { deleteTask, getTask, listTasks, retryTask } from "../api/client";
import type { LayoutContext } from "../components/Layout";
import type { TaskDetail, TaskSummary } from "../types";
import {
  Button,
  Card,
  EmptyState,
  formatBytes,
  IconRefresh,
  IconTrash,
  PageHeader,
  ProgressBar,
  StatePill,
  Tag,
} from "../components/ui";

type Filter = "all" | "running" | "completed" | "failed";

const TABS: { key: Filter; label: string }[] = [
  { key: "all", label: "全部" },
  { key: "running", label: "进行中" },
  { key: "completed", label: "已完成" },
  { key: "failed", label: "失败" },
];

const TERMINAL = new Set(["COMPLETED", "FAILED"]);

const STAGES: { label: string; min: number }[] = [
  { label: "排队", min: 5 },
  { label: "解析", min: 30 },
  { label: "已解析", min: 45 },
  { label: "切块", min: 60 },
  { label: "向量化", min: 75 },
  { label: "索引", min: 90 },
  { label: "完成", min: 100 },
];

export default function Tasks() {
  const { kbID } = useOutletContext<LayoutContext>();
  const [tasks, setTasks] = useState<TaskSummary[]>([]);
  const [filter, setFilter] = useState<Filter>("all");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<TaskDetail | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(() => {
    setLoading(true);
    setError(null);
    listTasks(undefined, 200, 0, kbID)
      .then((r) => setTasks(r.tasks))
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [kbID]);

  useEffect(() => {
    setTasks([]);
    setSelected(null);
    setSelectedId(null);
    refresh();
  }, [refresh]);

  // Poll while any task is still in-flight.
  useEffect(() => {
    const hasRunning = tasks.some((t) => !TERMINAL.has(t.status));
    if (!hasRunning) return;
    const id = setInterval(refresh, 4000);
    return () => clearInterval(id);
  }, [tasks, refresh]);

  useEffect(() => {
    if (!selectedId) return;
    let alive = true;
    getTask(selectedId, kbID)
      .then((d) => alive && setSelected(d))
      .catch(() => alive && setSelected(null));
    return () => { alive = false; };
  }, [selectedId, tasks, kbID]);

  const filtered = tasks.filter((t) => {
    if (filter === "running") return !TERMINAL.has(t.status);
    if (filter === "completed") return t.status === "COMPLETED";
    if (filter === "failed") return t.status === "FAILED";
    return true;
  });

  const handleDelete = useCallback(
    async (id: string) => {
      if (!window.confirm("确定删除该任务及其文件吗？此操作不可撤销。")) return;
      setBusy(true);
      setError(null);
      try {
        await deleteTask(id, kbID);
        if (selectedId === id) {
          setSelectedId(null);
          setSelected(null);
        }
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    },
    [refresh, selectedId, kbID],
  );

  const handleRetry = useCallback(
    async (id: string) => {
      setBusy(true);
      setError(null);
      try {
        await retryTask(id, kbID);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    },
    [refresh, kbID],
  );

  return (
    <div>
      <PageHeader
        title="任务中心"
        description="跟踪文件在解析、切块、向量化、索引各阶段的状态流转"
        actions={<Button variant="ghost" onClick={refresh}>刷新</Button>}
      />

      <div className="mb-3 flex gap-1">
        {TABS.map((tab) => (
          <button
            key={tab.key}
            onClick={() => setFilter(tab.key)}
            className={`rounded-lg px-3 py-1.5 text-xs transition ${
              filter === tab.key ? "bg-accent/10 text-accent" : "text-slate-400 hover:bg-white/5 hover:text-slate-200"
            }`}
          >
            {tab.label}
          </button>
        ))}
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[1fr_320px]">
        <Card title={`任务（${filtered.length}）`}>
          {error ? (
            <p className="py-8 text-center text-xs text-rose-300">{error}</p>
          ) : loading && tasks.length === 0 ? (
            <p className="py-8 text-center text-xs text-slate-500">加载中…</p>
          ) : filtered.length === 0 ? (
            <EmptyState title="无任务" hint="切换过滤条件或上传新文件。" />
          ) : (
            <ul className="divide-y divide-white/5">
              {filtered.map((t) => (
                <li
                  key={t.task_id}
                  onClick={() => setSelectedId(t.task_id)}
                  className={`cursor-pointer rounded-lg px-3 py-2.5 transition ${
                    selectedId === t.task_id ? "bg-accent/5 ring-1 ring-accent/20" : "hover:bg-white/[0.03]"
                  }`}
                >
                  <div className="flex items-center justify-between">
                    <div className="min-w-0">
                      <p className="truncate text-xs text-slate-200">{t.file_name || t.task_id}</p>
                      <p className="mt-0.5 font-mono text-[9px] text-slate-600">{t.task_id}</p>
                    </div>
                    <div className="flex items-center gap-2">
                      <StatePill state={t.status} />
                      <button
                        type="button"
                        title="删除任务"
                        disabled={busy}
                        onClick={(e) => {
                          e.stopPropagation();
                          void handleDelete(t.task_id);
                        }}
                        className="rounded p-1 text-slate-500 transition hover:bg-rose-500/10 hover:text-rose-300 disabled:opacity-40"
                      >
                        <IconTrash />
                      </button>
                    </div>
                  </div>
                  <div className="mt-2 flex items-center gap-2">
                    <ProgressBar value={t.progress} />
                    <span className="w-8 text-right text-[10px] text-slate-500">{t.progress}%</span>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card title="任务详情">
          {selected ? (
            <div className="space-y-3 text-xs">
              <div className="flex items-center justify-between gap-2">
                <StatePill state={selected.status} />
                <div className="flex items-center gap-1.5">
                  {selected.status === "FAILED" && (
                    <Button variant="ghost" disabled={busy} onClick={() => void handleRetry(selected.task_id)}>
                      <IconRefresh /> 重试
                    </Button>
                  )}
                  <Button variant="danger" disabled={busy} onClick={() => void handleDelete(selected.task_id)}>
                    <IconTrash /> 删除
                  </Button>
                </div>
              </div>
              <div>
                <div className="mb-1 flex justify-between text-slate-500">
                  <span>进度</span><span>{selected.progress}%</span>
                </div>
                <ProgressBar value={selected.progress} />
              </div>

              <div className="rounded-lg bg-white/[0.02] p-3">
                <p className="mb-2 text-[10px] uppercase tracking-wider text-slate-500">状态时间线</p>
                <div className="flex items-start">
                  {STAGES.map((s, i) => {
                    const reached = selected.status !== "FAILED" && selected.progress >= s.min;
                    const isLast = i === STAGES.length - 1;
                    return (
                      <div key={s.label} className="flex flex-1 flex-col items-center">
                        <div className="flex w-full items-center">
                          <div
                            className={`h-1.5 w-1.5 shrink-0 rounded-full ${
                              reached ? "bg-accent" : selected.status === "FAILED" ? "bg-rose-400" : "bg-slate-600"
                            }`}
                          />
                          {!isLast && (
                            <div className={`h-px flex-1 ${reached ? "bg-accent/40" : "bg-white/5"}`} />
                          )}
                        </div>
                        <span className={`mt-1 text-[9px] ${reached ? "text-slate-300" : "text-slate-600"}`}>{s.label}</span>
                      </div>
                    );
                  })}
                </div>
                {selected.status === "FAILED" && (
                  <p className="mt-2 text-[10px] text-rose-300">任务失败，可点击右上角「重试」重新触发解析。</p>
                )}
              </div>

              <div className="rounded-lg bg-white/[0.02] p-3">
                <p className="mb-1 text-[10px] uppercase tracking-wider text-slate-500">任务 ID</p>
                <p className="break-all font-mono text-[11px] text-slate-300">{selected.task_id}</p>
              </div>

              {selected.error_msg && (
                <div className="rounded-lg border border-rose-500/20 bg-rose-500/5 p-3 text-rose-200">
                  <p className="mb-1 text-[10px] uppercase tracking-wider text-rose-300/70">错误</p>
                  <p className="break-words">{selected.error_msg}</p>
                </div>
              )}

              <div className="space-y-1.5 border-t border-white/5 pt-3 text-[11px]">
                <div className="flex justify-between"><span className="text-slate-500">文件</span><span className="text-slate-300">{selected.file_name || "-"}</span></div>
                <div className="flex justify-between"><span className="text-slate-500">类型</span><Tag>{selected.file_type || "-"}</Tag></div>
                <div className="flex justify-between"><span className="text-slate-500">大小</span><span className="text-slate-300">{formatBytes(selected.file_size ?? 0)}</span></div>
                <div className="flex justify-between"><span className="text-slate-500">创建</span><span className="text-slate-400">{selected.created_at?.replace("T", " ").slice(0, 19) || "-"}</span></div>
                <div className="flex justify-between"><span className="text-slate-500">更新</span><span className="text-slate-400">{selected.updated_at?.replace("T", " ").slice(0, 19) || "-"}</span></div>
              </div>

              {selected.content_hash && (
                <div className="rounded-lg bg-white/[0.02] p-3">
                  <p className="mb-1 text-[10px] uppercase tracking-wider text-slate-500">内容哈希 (content_hash)</p>
                  <p className="break-all font-mono text-[11px] text-slate-300">{selected.content_hash}</p>
                </div>
              )}

              {selected.metadata && Object.keys(selected.metadata).length > 0 && (
                <div className="rounded-lg bg-white/[0.02] p-3">
                  <p className="mb-2 text-[10px] uppercase tracking-wider text-slate-500">元数据 (metadata)</p>
                  <dl className="space-y-1 text-[11px]">
                    {Object.entries(selected.metadata).map(([k, v]) => (
                      <div key={k} className="flex gap-2">
                        <dt className="w-24 shrink-0 text-slate-500">{k}</dt>
                        <dd className="min-w-0 flex-1 break-all text-slate-300">{v}</dd>
                      </div>
                    ))}
                  </dl>
                </div>
              )}
            </div>
          ) : (
            <EmptyState title="选择一个任务" hint="点击左侧任务查看详细状态与错误信息。" />
          )}
        </Card>
      </div>
    </div>
  );
}
