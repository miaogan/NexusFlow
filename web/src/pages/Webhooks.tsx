import { useCallback, useEffect, useState } from "react";
import {
  createWebhook,
  deleteWebhook,
  hasPermission,
  listWebhookDeliveries,
  listWebhooks,
} from "../api/client";
import type { WebhookDelivery, WebhookSubscription } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner, Tag } from "../components/ui";

const inputClass =
  "w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40";

const EVENTS = [
  "task.completed",
  "task.failed",
  "index.completed",
  "workflow.completed",
  "workflow.failed",
  "approval.decided",
  "*",
];

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function deliveryClass(status: string): string {
  switch (status) {
    case "SUCCESS":
      return "bg-emerald-500/10 text-emerald-300";
    case "FAILED":
      return "bg-rose-500/10 text-rose-300";
    default:
      return "bg-amber-500/10 text-amber-300";
  }
}

export default function Webhooks() {
  const canWrite = hasPermission("webhook:write");
  const [hooks, setHooks] = useState<WebhookSubscription[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [created, setCreated] = useState<{ name: string; secret: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const [form, setForm] = useState({
    name: "",
    targetURL: "",
    events: ["task.completed", "task.failed"],
    secret: "",
    isActive: true,
  });

  const [selected, setSelected] = useState<string | null>(null);
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([]);
  const [deliveryLoading, setDeliveryLoading] = useState(false);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await listWebhooks();
      setHooks(res.webhooks ?? []);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (canWrite) void reload();
  }, [canWrite, reload]);

  const flash = (msg: string) => {
    setNotice(msg);
    window.setTimeout(() => setNotice(null), 2500);
  };

  const toggleEvent = (event: string) =>
    setForm((f) => {
      if (event === "*") {
        return { ...f, events: f.events.includes("*") ? [] : ["*"] };
      }
      const next = f.events.includes(event)
        ? f.events.filter((x) => x !== event)
        : [...f.events.filter((x) => x !== "*"), event];
      return { ...f, events: next };
    });

  const submit = async () => {
    if (!form.name.trim() || !form.targetURL.trim() || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await createWebhook({
        name: form.name.trim(),
        target_url: form.targetURL.trim(),
        events: form.events,
        secret: form.secret.trim() || undefined,
        is_active: form.isActive,
      });
      setCreated({ name: res.webhook.name, secret: res.secret });
      setForm({ name: "", targetURL: "", events: ["task.completed", "task.failed"], secret: "", isActive: true });
      flash("Webhook 已创建");
      await reload();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (hook: WebhookSubscription) => {
    if (!window.confirm(`确认删除 Webhook「${hook.name}」？投递日志将一并删除。`)) return;
    try {
      await deleteWebhook(hook.id);
      if (selected === hook.id) {
        setSelected(null);
        setDeliveries([]);
      }
      flash("Webhook 已删除");
      await reload();
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const openDeliveries = async (hook: WebhookSubscription) => {
    if (selected === hook.id) {
      setSelected(null);
      setDeliveries([]);
      return;
    }
    setSelected(hook.id);
    setDeliveryLoading(true);
    setError(null);
    try {
      const res = await listWebhookDeliveries(hook.id);
      setDeliveries(res.deliveries ?? []);
    } catch (e) {
      setError(errMsg(e));
      setDeliveries([]);
    } finally {
      setDeliveryLoading(false);
    }
  };

  const copySecret = async () => {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.secret);
      flash("已复制到剪贴板");
    } catch {
      setError("复制失败，请手动选择文本复制");
    }
  };

  if (!canWrite) {
    return (
      <div>
        <PageHeader title="事件 Webhook" description="租户级事件订阅与投递日志" />
        <Card>
          <EmptyState title="无权访问" hint="当前账号缺少 webhook:write 权限。" />
        </Card>
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="事件 Webhook"
        description="订阅任务、索引、工作流与审批事件；HMAC-SHA256 签名，失败按指数退避重试"
        actions={
          <Button variant="ghost" onClick={() => reload()} disabled={loading}>
            刷新
          </Button>
        }
      />

      {error && <div className="panel mb-4 p-3 text-xs text-rose-300">{error}</div>}
      {notice && <div className="panel mb-4 p-3 text-xs text-emerald-300">{notice}</div>}

      {created && (
        <div className="panel mb-4 border border-amber-400/30 p-4">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <p className="text-xs font-medium text-amber-200">
                请保存「{created.name}」的签名密钥，关闭后无法再次查看
              </p>
              <code className="mt-2 block break-all rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-[11px] text-accent">
                {created.secret}
              </code>
            </div>
            <div className="flex gap-2">
              <Button variant="ghost" onClick={copySecret}>
                复制
              </Button>
              <Button variant="subtle" onClick={() => setCreated(null)}>
                我已保存
              </Button>
            </div>
          </div>
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-[1fr,360px]">
        <Card title={`订阅列表 · ${hooks.length}`}>
          {loading && hooks.length === 0 ? (
            <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
              <Spinner /> 加载中…
            </div>
          ) : hooks.length === 0 ? (
            <EmptyState title="暂无 Webhook" hint="在右侧创建订阅，网关会把匹配事件推送到目标 URL。" />
          ) : (
            <ul className="space-y-2">
              {hooks.map((hook) => (
                <li
                  key={hook.id}
                  className="rounded-lg border border-white/5 bg-white/[0.02] p-3"
                >
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-xs font-medium text-slate-200">{hook.name}</span>
                    <span
                      className={`rounded px-1.5 py-0.5 text-[9px] ${
                        hook.is_active ? "bg-emerald-500/10 text-emerald-300" : "bg-slate-500/10 text-slate-400"
                      }`}
                    >
                      {hook.is_active ? "ACTIVE" : "PAUSED"}
                    </span>
                    <span className="text-[10px] text-slate-500">
                      创建 {new Date(hook.created_at).toLocaleString()}
                    </span>
                    <div className="ml-auto flex gap-2">
                      <Button variant="subtle" onClick={() => openDeliveries(hook)} className="px-2 py-1 text-[10px]">
                        {selected === hook.id ? "收起日志" : "投递日志"}
                      </Button>
                      <Button variant="danger" onClick={() => remove(hook)} className="px-2 py-1 text-[10px]">
                        删除
                      </Button>
                    </div>
                  </div>
                  <code className="mt-1 block break-all text-[10px] text-slate-500">{hook.target_url}</code>
                  <div className="mt-1 flex flex-wrap gap-1">
                    {hook.events.map((event) => (
                      <Tag key={event}>{event}</Tag>
                    ))}
                  </div>

                  {selected === hook.id && (
                    <div className="mt-3 border-t border-white/5 pt-3">
                      {deliveryLoading ? (
                        <div className="flex items-center gap-2 text-xs text-slate-500">
                          <Spinner /> 加载投递日志…
                        </div>
                      ) : deliveries.length === 0 ? (
                        <p className="text-[11px] text-slate-500">暂无投递记录。</p>
                      ) : (
                        <ul className="space-y-1.5">
                          {deliveries.map((d) => (
                            <li key={d.id} className="rounded border border-white/5 px-2 py-1.5">
                              <div className="flex flex-wrap items-center gap-2 text-[10px]">
                                <span className={`rounded px-1.5 py-0.5 ${deliveryClass(d.status)}`}>{d.status}</span>
                                <span className="text-slate-300">{d.event_type}</span>
                                <span className="text-slate-500">尝试 {d.attempt_count}</span>
                                <span className="text-slate-500">HTTP {d.http_status || "-"}</span>
                                <span className="ml-auto text-slate-600">
                                  {new Date(d.created_at).toLocaleString()}
                                </span>
                              </div>
                              {d.response_body && (
                                <code className="mt-1 block break-all text-[10px] text-slate-500">{d.response_body}</code>
                              )}
                              <details className="mt-1">
                                <summary className="cursor-pointer text-[10px] text-slate-500">查看事件载荷</summary>
                                <pre className="mt-1 max-h-40 overflow-auto rounded bg-ink-950 p-2 text-[10px] text-slate-400">
                                  {JSON.stringify(d.payload, null, 2)}
                                </pre>
                              </details>
                            </li>
                          ))}
                        </ul>
                      )}
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card title="创建订阅">
          <div className="space-y-3">
            <input
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              placeholder="名称，如 ops-prod"
              className={inputClass}
            />
            <input
              value={form.targetURL}
              onChange={(e) => setForm({ ...form, targetURL: e.target.value })}
              placeholder="https://example.com/hooks/nexusflow"
              className={inputClass}
            />

            <div>
              <p className="mb-1.5 text-[10px] uppercase tracking-wider text-slate-500">事件</p>
              <div className="grid grid-cols-2 gap-1.5">
                {EVENTS.map((event) => (
                  <label key={event} className="flex cursor-pointer items-center gap-2 text-[11px] text-slate-300">
                    <input
                      type="checkbox"
                      checked={form.events.includes(event)}
                      onChange={() => toggleEvent(event)}
                      className="accent-[color:var(--accent)]"
                    />
                    <span>{event === "*" ? "全部事件" : event}</span>
                  </label>
                ))}
              </div>
            </div>

            <input
              value={form.secret}
              onChange={(e) => setForm({ ...form, secret: e.target.value })}
              placeholder="签名密钥（留空自动生成，≥16 位）"
              className={inputClass}
            />

            <label className="flex cursor-pointer items-center gap-2 text-xs text-slate-300">
              <input
                type="checkbox"
                checked={form.isActive}
                onChange={(e) => setForm({ ...form, isActive: e.target.checked })}
                className="accent-[color:var(--accent)]"
              />
              创建后立即启用
            </label>

            <p className="text-[10px] leading-relaxed text-slate-500">
              投递头包含 <code>X-NexusFlow-Signature: sha256=&lt;HMAC&gt;</code>，请对原始 body 校验。
            </p>

            <Button
              onClick={submit}
              disabled={busy || !form.name.trim() || !form.targetURL.trim() || form.events.length === 0}
              className="w-full"
            >
              {busy ? "创建中…" : "创建 Webhook"}
            </Button>
          </div>
        </Card>
      </div>
    </div>
  );
}
