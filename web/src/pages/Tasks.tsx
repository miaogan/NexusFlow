import { useCallback, useEffect, useState } from "react";
import { getTask, listTasks } from "../api/client";
import type { TaskDetail, TaskSummary } from "../types";
import {
  Button,
  Card,
  EmptyState,
  formatBytes,
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

export default function Tasks() {
  const [tasks, setTasks] = useState<TaskSummary[]>([]);
  const [filter, setFilter] = useState<Filter>("all");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<TaskDetail | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const refresh = useCallback(() => {
    setLoading(true);
    setError(null);
    listTasks(undefined, 200)
      .then((r) => setTasks(r.tasks))
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(refresh, [refresh]);

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
    getTask(selectedId)
      .then((d) => alive && setSelected(d))
      .catch(() => alive && setSelected(null));
    return () => { alive = false; };
  }, [selectedId, tasks]);

  const filtered = tasks.filter((t) => {
    if (filter === "running") return !TERMINAL.has(t.status);
    if (filter === "completed") return t.status === "COMPLETED";
    if (filter === "failed") return t.status === "FAILED";
    return true;
  });

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
                    <StatePill state={t.status} />
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
              <div className="flex items-center justify-between">
                <span className="text-slate-500">状态</span>
                <StatePill state={selected.status} />
              </div>
              <div>
                <div className="mb-1 flex justify-between text-slate-500">
                  <span>进度</span><span>{selected.progress}%</span>
                </div>
                <ProgressBar value={selected.progress} />
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
              {(() => {
                const full = tasks.find((t) => t.task_id === selected.task_id);
                if (!full) return null;
                return (
                  <div className="space-y-1.5 border-t border-white/5 pt-3 text-[11px]">
                    <div className="flex justify-between"><span className="text-slate-500">文件</span><span className="text-slate-300">{full.file_name}</span></div>
                    <div className="flex justify-between"><span className="text-slate-500">类型</span><Tag>{full.file_type}</Tag></div>
                    <div className="flex justify-between"><span className="text-slate-500">大小</span><span className="text-slate-300">{formatBytes(full.file_size)}</span></div>
                    <div className="flex justify-between"><span className="text-slate-500">创建</span><span className="text-slate-400">{full.created_at?.replace("T", " ").slice(0, 19)}</span></div>
                    <div className="flex justify-between"><span className="text-slate-500">更新</span><span className="text-slate-400">{full.updated_at?.replace("T", " ").slice(0, 19)}</span></div>
                  </div>
                );
              })()}
            </div>
          ) : (
            <EmptyState title="选择一个任务" hint="点击左侧任务查看详细状态与错误信息。" />
          )}
        </Card>
      </div>
    </div>
  );
}
