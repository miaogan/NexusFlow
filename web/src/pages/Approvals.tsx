import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  decideApproval,
  decideWorkflowRun,
  getWorkflowRun,
  hasPermission,
  listApprovals,
  listWorkflowRuns,
} from "../api/client";
import type { ToolApproval, WorkflowRunState } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner, Tag } from "../components/ui";

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function approvalClass(status: string): string {
  switch (status) {
    case "approved":
      return "bg-emerald-500/10 text-emerald-300";
    case "rejected":
      return "bg-rose-500/10 text-rose-300";
    default:
      return "bg-amber-500/10 text-amber-300";
  }
}

function runClass(status: string): string {
  switch (status) {
    case "COMPLETED":
      return "bg-emerald-500/10 text-emerald-300";
    case "FAILED":
    case "REJECTED":
      return "bg-rose-500/10 text-rose-300";
    case "WAITING_APPROVAL":
      return "bg-amber-500/10 text-amber-300";
    default:
      return "bg-sky-500/10 text-sky-300";
  }
}

function ago(seconds: number): string {
  if (!seconds) return "-";
  return new Date(seconds * 1000).toLocaleString();
}

function duration(ms: number): string {
  if (!ms) return "0 ms";
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)} s` : `${ms} ms`;
}

export default function Approvals() {
  const canRun = hasPermission("workflow:run");
  const [tab, setTab] = useState<"tools" | "runs">("tools");
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [approvals, setApprovals] = useState<ToolApproval[]>([]);
  const [approvalStatus, setApprovalStatus] = useState("pending");
  const [approvalLoading, setApprovalLoading] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);

  const [runs, setRuns] = useState<WorkflowRunState[]>([]);
  const [runLoading, setRunLoading] = useState(false);
  const [runStatus, setRunStatus] = useState("WAITING_APPROVAL");
  const [selectedRun, setSelectedRun] = useState<WorkflowRunState | null>(null);
  const [runBusy, setRunBusy] = useState(false);
  const [reason, setReason] = useState("");
  const [autoRefresh, setAutoRefresh] = useState(true);

  const flash = (msg: string) => {
    setNotice(msg);
    window.setTimeout(() => setNotice(null), 2500);
  };

  const loadApprovals = useCallback(async (status: string) => {
    setApprovalLoading(true);
    setError(null);
    try {
      const res = await listApprovals(status === "all" ? "" : status);
      setApprovals(res.approvals ?? []);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setApprovalLoading(false);
    }
  }, []);

  const loadRuns = useCallback(async (status: string) => {
    setRunLoading(true);
    setError(null);
    try {
      const res = await listWorkflowRuns(
        50,
        undefined,
        status === "all" ? undefined : status,
      );
      setRuns(res.runs ?? []);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setRunLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!canRun) return;
    if (tab === "tools") void loadApprovals(approvalStatus);
    else void loadRuns(runStatus);
  }, [canRun, tab, approvalStatus, runStatus, loadApprovals, loadRuns]);

  // Optional polling keeps the approval queue fresh without manual refresh.
  useEffect(() => {
    if (!canRun || !autoRefresh) return;
    const id = setInterval(() => {
      if (tab === "tools") {
        void loadApprovals(approvalStatus);
      } else {
        void loadRuns(runStatus);
        if (selectedRun && selectedRun.status === "WAITING_APPROVAL") {
          void openRun(selectedRun.id);
        }
      }
    }, 8000);
    return () => clearInterval(id);
  }, [
    canRun,
    autoRefresh,
    tab,
    approvalStatus,
    runStatus,
    selectedRun?.id,
    selectedRun?.status,
    loadApprovals,
    loadRuns,
  ]);

  const decideTool = async (approval: ToolApproval, decision: "approve" | "reject") => {
    setError(null);
    try {
      await decideApproval(approval.id, decision, reason.trim());
      setReason("");
      flash(decision === "approve" ? "已批准" : "已拒绝");
      await loadApprovals(approvalStatus);
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const openRun = async (id: string) => {
    setError(null);
    try {
      setSelectedRun(await getWorkflowRun(id));
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const decideRun = async (decision: "approve" | "reject") => {
    if (!selectedRun || runBusy) return;
    setRunBusy(true);
    setError(null);
    try {
      await decideWorkflowRun(selectedRun.id, decision, reason.trim());
      setReason("");
      // Approve returns a RunResult; re-read the persisted state for the detail.
      const updated = await getWorkflowRun(selectedRun.id);
      setSelectedRun(updated);
      flash(decision === "approve" ? "已批准并恢复运行" : "已拒绝运行");
      await loadRuns(runStatus);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setRunBusy(false);
    }
  };

  if (!canRun) {
    return (
      <div>
        <PageHeader title="审批中心" description="工具审批与工作流运行审批" />
        <Card>
          <EmptyState title="无权访问" hint="当前账号缺少 workflow:run 权限。" />
        </Card>
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="审批中心"
        description="处理高风险工具调用与挂起的工作流运行，审批结果会触发 approval.decided 事件"
        actions={
          <Button
            variant="ghost"
            onClick={() => (tab === "tools" ? loadApprovals(approvalStatus) : loadRuns(runStatus))}
          >
            刷新
          </Button>
        }
      />

      {error && <div className="panel mb-4 p-3 text-xs text-rose-300">{error}</div>}
      {notice && <div className="panel mb-4 p-3 text-xs text-emerald-300">{notice}</div>}

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <input
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder="审批备注（可选，批准/拒绝时一并记录）"
          className="w-72 rounded-lg border border-white/10 bg-ink-950 px-3 py-1.5 text-xs text-slate-200 outline-none focus:border-accent/40"
        />
        <label className="flex items-center gap-1.5 text-[10px] text-slate-400">
          <input
            type="checkbox"
            checked={autoRefresh}
            onChange={(event) => setAutoRefresh(event.target.checked)}
            className="accent-[color:var(--accent)]"
          />
          自动刷新
        </label>
      </div>

      <div className="mb-4 flex items-center gap-2">
        {(["tools", "runs"] as const).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`rounded-lg px-3 py-1.5 text-xs transition ${
              tab === t ? "bg-accent/15 text-accent" : "text-slate-400 hover:bg-white/5 hover:text-slate-200"
            }`}
          >
            {t === "tools" ? "工具审批" : "工作流运行"}
          </button>
        ))}
      </div>

      {tab === "tools" && (
        <Card
          title={`工具审批 · ${approvals.length}`}
          action={
            <div className="flex items-center gap-1">
              {[
                ["pending", "待审批"],
                ["approved", "已批准"],
                ["rejected", "已拒绝"],
                ["all", "全部"],
              ].map(([value, label]) => (
                <button
                  key={value}
                  onClick={() => setApprovalStatus(value)}
                  className={`rounded px-2 py-1 text-[10px] transition ${
                    approvalStatus === value
                      ? "bg-accent/15 text-accent"
                      : "text-slate-400 hover:bg-white/5"
                  }`}
                >
                  {label}
                </button>
              ))}
            </div>
          }
        >
          {approvalLoading && approvals.length === 0 ? (
            <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
              <Spinner /> 加载中…
            </div>
          ) : approvals.length === 0 ? (
            <EmptyState title="暂无审批记录" hint="高风险工具调用会在这里生成待审批记录。" />
          ) : (
            <ul className="space-y-2">
              {approvals.map((approval) => (
                <li key={approval.id} className="rounded-lg border border-white/5 bg-white/[0.02] p-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className={`rounded px-1.5 py-0.5 text-[9px] ${approvalClass(approval.status)}`}>
                      {approval.status}
                    </span>
                    <span className="text-xs font-medium text-slate-200">{approval.tool}</span>
                    <Tag>risk: {approval.risk}</Tag>
                    <Tag>{approval.user_id || "unknown"}</Tag>
                    <span className="text-[10px] text-slate-500">{ago(approval.created_at)}</span>
                    <div className="ml-auto flex gap-2">
                      <Button
                        variant="subtle"
                        onClick={() => setExpanded(expanded === approval.id ? null : approval.id)}
                        className="px-2 py-1 text-[10px]"
                      >
                        {expanded === approval.id ? "收起参数" : "查看参数"}
                      </Button>
                      {approval.status === "pending" && (
                        <>
                          <Button
                            onClick={() => decideTool(approval, "approve")}
                            className="px-2 py-1 text-[10px]"
                          >
                            批准
                          </Button>
                          <Button
                            variant="danger"
                            onClick={() => decideTool(approval, "reject")}
                            className="px-2 py-1 text-[10px]"
                          >
                            拒绝
                          </Button>
                        </>
                      )}
                    </div>
                  </div>
                  {approval.trace_id && (
                    <p className="mt-1 text-[10px] text-slate-500">trace: {approval.trace_id}</p>
                  )}
                  {expanded === approval.id && (
                    <pre className="mt-2 max-h-48 overflow-auto rounded bg-ink-950 p-2 text-[10px] text-slate-400">
                      {JSON.stringify(approval.args, null, 2)}
                    </pre>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Card>
      )}

      {tab === "runs" && (
        <div className="grid gap-4 lg:grid-cols-[380px,1fr]">
          <Card
            title={`运行列表 · ${runs.length}`}
            action={
              <div className="flex flex-wrap items-center gap-1">
                {[
                  ["WAITING_APPROVAL", "待审批"],
                  ["COMPLETED", "已完成"],
                  ["FAILED", "失败"],
                  ["REJECTED", "已拒绝"],
                  ["all", "全部"],
                ].map(([value, label]) => (
                  <button
                    key={value}
                    onClick={() => setRunStatus(value)}
                    className={`rounded px-2 py-1 text-[10px] transition ${
                      runStatus === value
                        ? "bg-accent/15 text-accent"
                        : "text-slate-400 hover:bg-white/5"
                    }`}
                  >
                    {label}
                  </button>
                ))}
              </div>
            }
          >
            {runLoading && runs.length === 0 ? (
              <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
                <Spinner /> 加载中…
              </div>
            ) : runs.length === 0 ? (
              <EmptyState title="暂无运行记录" hint="创建包含 human_approval 节点的 DAG 并运行后，可在此审批。" />
            ) : (
              <ul className="space-y-1.5">
                {runs.map((run) => (
                  <li key={run.id}>
                    <button
                      onClick={() => void openRun(run.id)}
                      className={`w-full rounded-lg border px-3 py-2 text-left transition ${
                        selectedRun?.id === run.id
                          ? "border-accent/40 bg-accent/5"
                          : "border-white/5 bg-white/[0.02] hover:border-white/15"
                      }`}
                    >
                      <div className="flex items-center gap-2">
                        <span className="text-xs font-medium text-slate-200">{run.dag?.name || "DAG"}</span>
                        <span className={`rounded px-1.5 py-0.5 text-[9px] ${runClass(run.status)}`}>
                          {run.status}
                        </span>
                      </div>
                      <p className="mt-1 font-mono text-[10px] text-slate-500">{run.id}</p>
                      <p className="mt-1 text-[10px] text-slate-500">
                        {new Date(run.created_at).toLocaleString()}
                        {run.pending_node ? ` · 等待 ${run.pending_node}` : ""}
                      </p>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card title="运行详情">
            {!selectedRun ? (
              <EmptyState title="选择一条运行" hint="点击左侧运行查看节点结果、耗时与审批操作。" />
            ) : (
              <div className="space-y-4">
                <div className="flex flex-wrap items-center gap-2">
                  <span className={`rounded px-2 py-0.5 text-[10px] ${runClass(selectedRun.status)}`}>
                    {selectedRun.status}
                  </span>
                  <span className="text-sm font-medium text-slate-200">{selectedRun.dag?.name}</span>
                  {selectedRun.definition_id && (
                    <Link
                      to={`/workflow?definition=${encodeURIComponent(selectedRun.definition_id)}`}
                      className="text-[10px] text-accent hover:underline"
                    >
                      打开工作流
                    </Link>
                  )}
                  {selectedRun.pending_node && <Tag>等待节点 {selectedRun.pending_node}</Tag>}
                </div>
                <div className="grid gap-1 text-[10px] text-slate-500 sm:grid-cols-2">
                  <span className="break-all">ID: {selectedRun.id}</span>
                  <span>创建: {new Date(selectedRun.created_at).toLocaleString()}</span>
                  <span>更新: {new Date(selectedRun.updated_at).toLocaleString()}</span>
                  <span>触发人: {selectedRun.meta?.UserID || "-"}</span>
                  {selectedRun.approved_by && <span>审批人: {selectedRun.approved_by}</span>}
                  {selectedRun.approval_reason && <span>审批原因: {selectedRun.approval_reason}</span>}
                </div>
                {selectedRun.error && (
                  <div className="rounded-lg border border-rose-500/30 bg-rose-500/5 p-2 text-[11px] text-rose-300">
                    {selectedRun.error}
                  </div>
                )}

                {selectedRun.status === "WAITING_APPROVAL" && (
                  <div className="flex gap-2">
                    <Button onClick={() => decideRun("approve")} disabled={runBusy}>
                      {runBusy ? "处理中…" : "批准并恢复"}
                    </Button>
                    <Button variant="danger" onClick={() => decideRun("reject")} disabled={runBusy}>
                      拒绝
                    </Button>
                  </div>
                )}

                <div>
                  <p className="mb-2 text-[10px] uppercase tracking-wider text-slate-500">节点结果</p>
                  {(!selectedRun.results || selectedRun.results.length === 0) ? (
                    <p className="text-[11px] text-slate-500">暂无节点结果。</p>
                  ) : (
                    <ol className="space-y-1.5">
                      {selectedRun.results.map((node) => (
                        <li key={node.node_id} className="rounded-lg border border-white/5 px-3 py-2">
                          <div className="flex flex-wrap items-center gap-2 text-[11px]">
                            <span
                              className={`h-1.5 w-1.5 rounded-full ${
                                node.error ? "bg-rose-400" : "bg-emerald-400"
                              }`}
                            />
                            <span className="font-medium text-slate-200">{node.node_id}</span>
                            <span className={`text-[10px] ${node.error ? "text-rose-300" : "text-slate-500"}`}>
                              {node.error ? "FAILED" : "OK"}
                            </span>
                            <span className="ml-auto text-[10px] text-slate-500">
                              {duration(node.duration / 1_000_000)}
                            </span>
                          </div>
                          {node.error && (
                            <p className="mt-1 text-[10px] text-rose-300">{node.error}</p>
                          )}
                          {node.output && (
                            <details className="mt-1">
                              <summary className="cursor-pointer text-[10px] text-slate-500">输出</summary>
                              <pre className="mt-1 max-h-40 overflow-auto rounded bg-ink-950 p-2 text-[10px] text-slate-400">
                                {JSON.stringify(node.output, null, 2)}
                              </pre>
                            </details>
                          )}
                        </li>
                      ))}
                    </ol>
                  )}
                  {selectedRun.skipped && selectedRun.skipped.length > 0 && (
                    <div className="mt-2 flex flex-wrap items-center gap-1">
                      <span className="text-[10px] text-slate-500">跳过：</span>
                      {selectedRun.skipped.map((id) => (
                        <Tag key={id}>{id}</Tag>
                      ))}
                    </div>
                  )}
                </div>
              </div>
            )}
          </Card>
        </div>
      )}
    </div>
  );
}
