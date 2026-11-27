import { useCallback, useEffect, useState } from "react";
import { Link, useOutletContext } from "react-router-dom";
import { listTasks } from "../api/client";
import type { LayoutContext } from "../components/Layout";
import type { TaskSummary } from "../types";
import { Button, Card, EmptyState, PageHeader, ProgressBar, StatePill } from "../components/ui";

function StatCard({ label, value, accent }: { label: string; value: number | string; accent: string }) {
  return (
    <div className="panel p-5">
      <p className="text-[10px] uppercase tracking-wider text-slate-500">{label}</p>
      <p className={`mt-2 text-3xl font-semibold ${accent}`}>{value}</p>
    </div>
  );
}

export default function Dashboard() {
  const { kbID } = useOutletContext<LayoutContext>();
  const [tasks, setTasks] = useState<TaskSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(() => {
    setLoading(true);
    setError(null);
    listTasks(undefined, 100, 0, kbID)
      .then((r) => setTasks(r.tasks))
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [kbID]);

  useEffect(() => {
    setTasks([]);
    refresh();
  }, [refresh]);

  const total = tasks.length;
  const done = tasks.filter((t) => t.status === "COMPLETED").length;
  const failed = tasks.filter((t) => t.status === "FAILED").length;
  const running = total - done - failed;
  const recent = tasks.slice(0, 6);

  return (
    <div>
      <PageHeader
        title="概览"
        description="文件向量化管道的整体运行状态"
        actions={
          <>
            <Link to="/files">
              <Button>上传文件</Button>
            </Link>
            <Button variant="ghost" onClick={refresh}>
              刷新
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <StatCard label="任务总数" value={total} accent="text-slate-100" />
        <StatCard label="进行中" value={running} accent="text-amber-300" />
        <StatCard label="已完成" value={done} accent="text-emerald-300" />
        <StatCard label="失败" value={failed} accent="text-rose-300" />
      </div>

      <div className="mt-5 grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card title="最近任务" className="lg:col-span-2" action={<Link to="/tasks" className="text-[11px] text-accent hover:underline">查看全部</Link>}>
          {error ? (
            <p className="py-8 text-center text-xs text-rose-300">{error}</p>
          ) : loading && tasks.length === 0 ? (
            <p className="py-8 text-center text-xs text-slate-500">加载中…</p>
          ) : recent.length === 0 ? (
            <EmptyState title="还没有任务" hint="去「文件管理」上传第一个文件，启动向量化管道。" />
          ) : (
            <ul className="divide-y divide-white/5">
              {recent.map((t) => (
                <li key={t.task_id} className="flex items-center justify-between py-2.5">
                  <div className="min-w-0">
                    <p className="truncate text-xs text-slate-200">{t.file_name || t.task_id}</p>
                    <p className="mt-0.5 font-mono text-[10px] text-slate-600">{t.task_id.slice(0, 8)}</p>
                  </div>
                  <div className="flex items-center gap-3">
                    <div className="w-24"><ProgressBar value={t.progress} /></div>
                    <StatePill state={t.status} />
                  </div>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card title="快捷入口">
          <div className="grid grid-cols-1 gap-2">
            <Link to="/files"><Button variant="subtle" className="w-full justify-start">文件上传与管理</Button></Link>
            <Link to="/tasks"><Button variant="subtle" className="w-full justify-start">任务状态跟踪</Button></Link>
            <Link to="/search"><Button variant="subtle" className="w-full justify-start">多路召回检索</Button></Link>
            <Link to="/sandbox"><Button variant="subtle" className="w-full justify-start">沙箱代码执行</Button></Link>
            <Link to="/workflow"><Button variant="subtle" className="w-full justify-start">DAG 工作流编排</Button></Link>
          </div>
        </Card>
      </div>
    </div>
  );
}
