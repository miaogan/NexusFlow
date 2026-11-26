import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { listTasks, uploadFile } from "../api/client";
import type { TaskSummary, UploadResult } from "../types";
import {
  Button,
  Card,
  EmptyState,
  formatBytes,
  PageHeader,
  ProgressBar,
  Spinner,
  StatePill,
  Tag,
} from "../components/ui";

export default function Files() {
  const [tasks, setTasks] = useState<TaskSummary[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadResult, setUploadResult] = useState<UploadResult | null>(null);
  const [dragOver, setDragOver] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(() => {
    setLoading(true);
    setError(null);
    listTasks(undefined, 100)
      .then((r) => setTasks(r.tasks))
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(refresh, [refresh]);

  const handleFiles = async (files: FileList | null) => {
    if (!files || files.length === 0) return;
    setUploading(true);
    setError(null);
    setUploadResult(null);
    try {
      for (const file of Array.from(files)) {
        const result = await uploadFile(file, { source: "web-console" });
        setUploadResult(result);
      }
      refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setUploading(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="文件管理"
        description="上传异构文件，系统将自动完成解析、切块、向量化与索引"
        actions={<Button variant="ghost" onClick={refresh}>刷新</Button>}
      />

      <Card title="上传文件">
        <div
          onDragOver={(e) => { e.preventDefault(); setDragOver(true); }}
          onDragLeave={() => setDragOver(false)}
          onDrop={(e) => { e.preventDefault(); setDragOver(false); handleFiles(e.dataTransfer.files); }}
          onClick={() => inputRef.current?.click()}
          className={`flex cursor-pointer flex-col items-center justify-center rounded-xl border-2 border-dashed py-10 transition ${
            dragOver ? "border-accent/50 bg-accent/5" : "border-white/10 hover:border-white/20"
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
        <Card title={`文件列表（${tasks.length}）`}>
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
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
