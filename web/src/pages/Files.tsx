import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useOutletContext } from "react-router-dom";
import { deleteFile, listTasks, reindexFile, uploadFile } from "../api/client";
import type { LayoutContext } from "../components/Layout";
import type { TaskSummary, UploadResult } from "../types";
import {
  Button,
  Card,
  EmptyState,
  formatBytes,
  IconTrash,
  PageHeader,
  Pager,
  ProgressBar,
  Spinner,
  StatePill,
  Tag,
} from "../components/ui";

const PAGE_SIZE = 20;

const STATUS_TABS: { key: string; label: string }[] = [
  { key: "", label: "全部" },
  { key: "RUNNING", label: "进行中" },
  { key: "COMPLETED", label: "已完成" },
  { key: "FAILED", label: "失败" },
];

export default function Files() {
  const { kbID, kbRole, kbReadOnly } = useOutletContext<LayoutContext>();
  const canWrite = !kbReadOnly && (kbRole === "kb:admin" || kbRole === "kb:editor");
  const [tasks, setTasks] = useState<TaskSummary[]>([]);
  const [status, setStatus] = useState("");
  const [page, setPage] = useState(0);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadResult, setUploadResult] = useState<UploadResult | null>(null);
  const [dragOver, setDragOver] = useState(false);
  const [busy, setBusy] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(() => {
    setLoading(true);
    setError(null);
    listTasks(status || undefined, PAGE_SIZE, page * PAGE_SIZE, kbID)
      .then((r) => {
        setTasks(r.tasks);
        setTotal(r.total);
      })
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [kbID, status, page]);

  useEffect(() => {
    setTasks([]);
    setUploadResult(null);
    refresh();
  }, [refresh]);

  const handleFiles = async (files: FileList | null) => {
    if (!canWrite || !files || files.length === 0) return;
    setUploading(true);
    setError(null);
    setUploadResult(null);
    try {
      for (const file of Array.from(files)) {
        const result = await uploadFile(file, { source: "web-console" }, kbID);
        setUploadResult(result);
      }
      if (page === 0) refresh();
      else setPage(0);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setUploading(false);
    }
  };

  const handleDelete = useCallback(
    async (id: string) => {
      if (!window.confirm("确定删除该文件吗？将同时清理对象存储、向量与图谱索引，此操作不可撤销。")) return;
      setBusy(true);
      setError(null);
      try {
        await deleteFile(id, kbID);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(false);
      }
    },
    [refresh, kbID],
  );

  const handleReindex = useCallback(
    async (id: string) => {
      setBusy(true);
      setError(null);
      try {
        await reindexFile(id, kbID);
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
        title="文件管理"
        description={
          kbReadOnly
            ? "引用型知识库：文件与向量来自源知识库，此处只读"
            : "上传异构文件，系统将自动完成解析、切块、向量化与索引"
        }
        actions={<Button variant="ghost" onClick={refresh}>刷新</Button>}
      />

      {kbReadOnly && (
        <div className="panel mb-4 p-3 text-xs text-sky-300">
          当前为引用型知识库，检索直接复用源库已向量化的数据；请切换到源知识库上传或删除文件。
        </div>
      )}

      <div className="mb-3 flex flex-wrap items-center gap-1">
        {STATUS_TABS.map((tab) => (
          <button
            key={tab.key}
            type="button"
            onClick={() => {
              setStatus(tab.key);
              setPage(0);
            }}
            className={`rounded-lg px-3 py-1.5 text-xs transition ${
              status === tab.key
                ? "bg-accent/10 text-accent"
                : "text-slate-400 hover:bg-white/5 hover:text-slate-200"
            }`}
          >
            {tab.label}
          </button>
        ))}
        {loading && <Spinner className="ml-2 h-3 w-3" />}
      </div>

      <Card title="上传文件">
        <div
          onDragOver={(e) => { e.preventDefault(); setDragOver(true); }}
          onDragLeave={() => setDragOver(false)}
          onDrop={(e) => { e.preventDefault(); setDragOver(false); canWrite && handleFiles(e.dataTransfer.files); }}
          onClick={() => canWrite && inputRef.current?.click()}
          className={`flex flex-col items-center justify-center rounded-xl border-2 border-dashed py-10 transition ${
            !canWrite
              ? "cursor-not-allowed border-white/5 opacity-60"
              : dragOver
                ? "cursor-pointer border-accent/50 bg-accent/5"
                : "cursor-pointer border-white/10 hover:border-white/20"
          }`}
        >
          <input
            ref={inputRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => handleFiles(e.target.files)}
          />
          {uploading ? (
            <><Spinner className="mb-2" /><p className="text-xs text-slate-400">上传中…</p></>
          ) : !canWrite ? (
            <>
              <p className="text-sm text-slate-300">
                {kbReadOnly ? "引用型知识库为只读" : "当前知识库为只读角色"}
              </p>
              <p className="mt-1 text-[10px] text-slate-500">
                {kbReadOnly
                  ? "文件与向量来自源知识库，请切换到源库上传"
                  : "需要编辑者或管理员权限才能上传文件"}
              </p>
            </>
          ) : (
            <>
              <p className="text-sm text-slate-200">点击或拖拽文件到此处</p>
              <p className="mt-1 text-[10px] text-slate-500">支持 PDF / DOCX / TXT / MD / HTML，单文件最大 100MB</p>
            </>
          )}
        </div>

        {uploadResult && (
          <div className="mt-3 rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-3 text-xs text-emerald-200">
            已创建任务 <span className="font-mono">{uploadResult.task_id.slice(0, 8)}</span>
            {uploadResult.content_hash && (
              <span className="ml-3 text-[10px] text-slate-400">hash: {uploadResult.content_hash.slice(0, 12)}…</span>
            )}
            <Link to="/tasks" className="ml-3 text-accent hover:underline">查看进度 →</Link>
          </div>
        )}
        {error && <p className="mt-3 text-xs text-rose-300">{error}</p>}
      </Card>

      <div className="mt-4">
        <Card title={`文件列表（${total}）`}>
          {loading && tasks.length === 0 ? (
            <p className="py-8 text-center text-xs text-slate-500">加载中…</p>
          ) : tasks.length === 0 ? (
            <EmptyState title="暂无文件" hint="上传文件后会在此显示处理状态。" />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-left text-xs">
                <thead>
                  <tr className="border-b border-white/5 text-[10px] uppercase tracking-wider text-slate-500">
                    <th className="py-2 pr-3">文件名</th>
                    <th className="px-3 py-2">类型</th>
                    <th className="px-3 py-2">大小</th>
                    <th className="px-3 py-2">进度</th>
                    <th className="px-3 py-2">状态</th>
                    <th className="px-3 py-2">创建时间</th>
                    <th className="px-3 py-2 text-right">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {tasks.map((t) => (
                    <tr key={t.task_id} className="border-b border-white/5">
                      <td className="py-2.5 pr-3">
                        <p className="truncate text-slate-200" style={{ maxWidth: 280 }}>{t.file_name || t.task_id}</p>
                        <p className="font-mono text-[9px] text-slate-600">{t.task_id.slice(0, 8)}</p>
                      </td>
                      <td className="px-3 py-2.5"><Tag>{t.file_type || "-"}</Tag></td>
                      <td className="px-3 py-2.5 text-slate-400">{formatBytes(t.file_size)}</td>
                      <td className="px-3 py-2.5"><div className="w-28"><ProgressBar value={t.progress} /></div></td>
                      <td className="px-3 py-2.5"><StatePill state={t.status} /></td>
                      <td className="px-3 py-2.5 text-slate-500">{t.created_at?.replace("T", " ").slice(0, 16)}</td>
                      <td className="px-3 py-2.5 text-right">
                        {canWrite && (
                          <div className="inline-flex items-center gap-1">
                            {t.status === "COMPLETED" && (
                              <button
                                type="button"
                                title="从已解析内容重建索引"
                                disabled={busy}
                                onClick={() => void handleReindex(t.task_id)}
                                className="inline-flex items-center gap-1 rounded px-2 py-1 text-[10px] text-sky-300 transition hover:bg-sky-500/10 disabled:opacity-40"
                              >
                                重建索引
                              </button>
                            )}
                            <button
                              type="button"
                              title="删除文件"
                              disabled={busy}
                              onClick={() => void handleDelete(t.task_id)}
                              className="inline-flex items-center gap-1 rounded px-2 py-1 text-[10px] text-rose-300 transition hover:bg-rose-500/10 disabled:opacity-40"
                            >
                              <IconTrash /> 删除
                            </button>
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <Pager
            page={page}
            pageSize={PAGE_SIZE}
            total={total}
            loading={loading}
            onChange={setPage}
          />
        </Card>
      </div>
    </div>
  );
}
