import { useCallback, useEffect, useMemo, useState } from "react";
import {
  createAPIKey,
  hasPermission,
  listAPIKeys,
  listKnowledgeBases,
  revokeAPIKey,
} from "../api/client";
import type { APIKey, KnowledgeBase } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner, Tag } from "../components/ui";

const inputClass =
  "w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40";

const SCOPES = [
  { key: "chat:completions", label: "Chat Completions", hint: "OpenAI 兼容问答（隐含检索）" },
  { key: "search:use", label: "Search", hint: "检索与媒体读取" },
];

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

function statusClass(status: string): string {
  return status === "ACTIVE"
    ? "bg-emerald-500/10 text-emerald-300"
    : "bg-rose-500/10 text-rose-300";
}

export default function ApiKeys() {
  const canWrite = hasPermission("apikey:write");
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [kbs, setKbs] = useState<KnowledgeBase[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [created, setCreated] = useState<{ name: string; key: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const [form, setForm] = useState({
    name: "",
    scopes: ["chat:completions", "search:use"],
    allowedKBIDs: [] as string[],
    rpm: 60,
    expiresDays: 0,
  });

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await listAPIKeys();
      setKeys(res.api_keys ?? []);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!canWrite) return;
    void reload();
    if (hasPermission("kb:read")) {
      listKnowledgeBases()
        .then((res) => setKbs(res.knowledge_bases ?? []))
        .catch(() => setKbs([]));
    }
  }, [canWrite, reload]);

  const flash = (msg: string) => {
    setNotice(msg);
    window.setTimeout(() => setNotice(null), 2500);
  };

  const toggleScope = (key: string) =>
    setForm((f) => ({
      ...f,
      scopes: f.scopes.includes(key)
        ? f.scopes.filter((s) => s !== key)
        : [...f.scopes, key],
    }));

  const toggleKB = (id: string) =>
    setForm((f) => ({
      ...f,
      allowedKBIDs: f.allowedKBIDs.includes(id)
        ? f.allowedKBIDs.filter((x) => x !== id)
        : [...f.allowedKBIDs, id],
    }));

  const submit = async () => {
    if (!form.name.trim() || form.scopes.length === 0 || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await createAPIKey({
        name: form.name.trim(),
        scopes: form.scopes,
        allowed_kb_ids: form.allowedKBIDs,
        rate_limit_rpm: form.rpm,
        expires_in_days: form.expiresDays,
      });
      setCreated({ name: res.api_key.name, key: res.key });
      setForm({
        name: "",
        scopes: ["chat:completions", "search:use"],
        allowedKBIDs: [],
        rpm: 60,
        expiresDays: 0,
      });
      flash("API Key 已创建");
      await reload();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (key: APIKey) => {
    if (!window.confirm(`确认吊销 API Key「${key.name}」？吊销后立即失效。`)) return;
    try {
      await revokeAPIKey(key.id);
      flash("API Key 已吊销");
      await reload();
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const copyKey = async () => {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.key);
      flash("已复制到剪贴板");
    } catch {
      setError("复制失败，请手动选择文本复制");
    }
  };

  const kbNames = useMemo(() => {
    const map = new Map(kbs.map((kb) => [kb.id, kb.name]));
    return (id: string) => map.get(id) ?? id;
  }, [kbs]);

  if (!canWrite) {
    return (
      <div>
        <PageHeader title="API Key" description="签发与管理对外集成密钥" />
        <Card>
          <EmptyState title="无权访问" hint="当前账号缺少 apikey:write 权限。" />
        </Card>
      </div>
    );
  }

  return (
    <div>
      <PageHeader
        title="API Key"
        description="租户级对外密钥，可绑定知识库与作用域；明文仅创建时返回一次"
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
                请立即保存密钥「{created.name}」，关闭后无法再次查看明文
              </p>
              <code className="mt-2 block break-all rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-[11px] text-accent">
                {created.key}
              </code>
            </div>
            <div className="flex gap-2">
              <Button variant="ghost" onClick={copyKey}>
                复制
              </Button>
              <Button variant="subtle" onClick={() => setCreated(null)}>
                我已保存
              </Button>
            </div>
          </div>
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-[1fr,340px]">
        <Card title={`密钥列表 · ${keys.length}`}>
          {loading && keys.length === 0 ? (
            <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
              <Spinner /> 加载中…
            </div>
          ) : keys.length === 0 ? (
            <EmptyState title="暂无 API Key" hint="在右侧签发密钥，用于 OpenAI 兼容端点或检索集成。" />
          ) : (
            <ul className="space-y-2">
              {keys.map((key) => (
                <li
                  key={key.id}
                  className="flex flex-wrap items-center gap-2 rounded-lg border border-white/5 bg-white/[0.02] p-3"
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-xs font-medium text-slate-200">{key.name}</span>
                      <span className={`rounded px-1.5 py-0.5 text-[9px] ${statusClass(key.status)}`}>
                        {key.status}
                      </span>
                      <code className="text-[10px] text-slate-500">{key.key_prefix}…</code>
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-1">
                      {key.scopes.map((scope) => (
                        <Tag key={scope}>{scope}</Tag>
                      ))}
                      {key.allowed_kb_ids.length === 0 ? (
                        <Tag>全部知识库</Tag>
                      ) : (
                        key.allowed_kb_ids.map((id) => <Tag key={id}>{kbNames(id)}</Tag>)
                      )}
                      <Tag>{key.rate_limit_rpm} RPM</Tag>
                      {key.expires_at && <Tag>到期 {new Date(key.expires_at).toLocaleString()}</Tag>}
                    </div>
                    <p className="mt-1 text-[10px] text-slate-500">
                      创建 {new Date(key.created_at).toLocaleString()}
                      {key.last_used_at ? ` · 最近使用 ${new Date(key.last_used_at).toLocaleString()}` : " · 尚未使用"}
                    </p>
                  </div>
                  {key.status === "ACTIVE" && (
                    <Button variant="danger" onClick={() => revoke(key)} className="px-2 py-1 text-[10px]">
                      吊销
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card title="签发新密钥">
          <div className="space-y-3">
            <input
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              placeholder="名称，如 ci-prod"
              className={inputClass}
            />

            <div>
              <p className="mb-1.5 text-[10px] uppercase tracking-wider text-slate-500">作用域</p>
              <div className="space-y-1.5">
                {SCOPES.map((scope) => (
                  <label key={scope.key} className="flex cursor-pointer items-start gap-2 text-xs text-slate-300">
                    <input
                      type="checkbox"
                      checked={form.scopes.includes(scope.key)}
                      onChange={() => toggleScope(scope.key)}
                      className="mt-0.5 accent-[color:var(--accent)]"
                    />
                    <span>
                      <span className="font-medium">{scope.label}</span>
                      <span className="ml-1 text-[10px] text-slate-500">{scope.hint}</span>
                    </span>
                  </label>
                ))}
              </div>
            </div>

            <div>
              <p className="mb-1.5 text-[10px] uppercase tracking-wider text-slate-500">
                允许的知识库（不选即租户内全部）
              </p>
              {kbs.length === 0 ? (
                <p className="text-[10px] text-slate-500">暂无可选知识库</p>
              ) : (
                <div className="max-h-32 space-y-1 overflow-auto rounded-lg border border-white/5 p-2">
                  {kbs.map((kb) => (
                    <label key={kb.id} className="flex cursor-pointer items-center gap-2 text-xs text-slate-300">
                      <input
                        type="checkbox"
                        checked={form.allowedKBIDs.includes(kb.id)}
                        onChange={() => toggleKB(kb.id)}
                        className="accent-[color:var(--accent)]"
                      />
                      <span>{kb.name}</span>
                      <span className="text-[10px] text-slate-600">{kb.id}</span>
                    </label>
                  ))}
                </div>
              )}
            </div>

            <div className="grid grid-cols-2 gap-2">
              <label className="text-xs text-slate-400">
                每分钟请求
                <input
                  type="number"
                  min={1}
                  max={6000}
                  value={form.rpm}
                  onChange={(e) => setForm({ ...form, rpm: Number(e.target.value) || 0 })}
                  className={`${inputClass} mt-1`}
                />
              </label>
              <label className="text-xs text-slate-400">
                有效期（天，0 永久）
                <input
                  type="number"
                  min={0}
                  max={3650}
                  value={form.expiresDays}
                  onChange={(e) => setForm({ ...form, expiresDays: Number(e.target.value) || 0 })}
                  className={`${inputClass} mt-1`}
                />
              </label>
            </div>

            <Button onClick={submit} disabled={busy || !form.name.trim() || form.scopes.length === 0} className="w-full">
              {busy ? "签发中…" : "签发 API Key"}
            </Button>
          </div>
        </Card>
      </div>
    </div>
  );
}
