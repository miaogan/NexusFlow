import { useCallback, useEffect, useState } from "react";
import { NavLink, Outlet, useNavigate } from "react-router-dom";
import {
  clearAuth,
  createKnowledgeBase,
  deleteKnowledgeBase,
  getActiveKB,
  getCurrentUser,
  getMe,
  getToken,
  grantKnowledgeBasePermission,
  hasPermission,
  isAnonymous,
  listKnowledgeBasePermissions,
  listKnowledgeBases,
  revokeKnowledgeBasePermission,
  setActiveKB,
  updateKnowledgeBase,
} from "../api/client";
import type { KnowledgeBase, KnowledgeBasePermission, MeInfo } from "../types";
import { KB_ROLE_LABELS } from "../types";
import { Button, Spinner } from "./ui";

type NavItem = { to: string; label: string; end?: boolean };

const NAV: NavItem[] = [
  { to: "/", label: "概览", end: true },
  { to: "/files", label: "文件管理" },
  { to: "/tasks", label: "任务中心" },
  { to: "/search", label: "检索测试" },
  { to: "/chat", label: "智能问答" },
  { to: "/workflow", label: "工作流编排" },
  { to: "/sandbox", label: "沙箱 Playground" },
];

export type LayoutContext = { kbID: string; kbRole: string };

export default function Layout() {
  const navigate = useNavigate();
  const [me, setMe] = useState<MeInfo | null>(null);
  const [ready, setReady] = useState(false);
  const [kbs, setKbs] = useState<KnowledgeBase[]>([]);
  const [kbID, setKbID] = useState<string>(getActiveKB());
  const [kbLoading, setKbLoading] = useState(false);
  const [managing, setManaging] = useState(false);
  const [kbError, setKbError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [newName, setNewName] = useState("");
  const [newDesc, setNewDesc] = useState("");
  const [editID, setEditID] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editDesc, setEditDesc] = useState("");
  const [perms, setPerms] = useState<KnowledgeBasePermission[]>([]);
  const [permSubjectType, setPermSubjectType] = useState("user");
  const [permSubjectID, setPermSubjectID] = useState("");
  const [permRole, setPermRole] = useState("kb:viewer");

  const user = getCurrentUser();
  const anon = isAnonymous();
  const canReadKB = hasPermission("kb:read") || anon;
  const canWriteKB = hasPermission("kb:write");

  const loadKBs = useCallback(async () => {
    if (!canReadKB) return;
    setKbLoading(true);
    try {
      const res = await listKnowledgeBases();
      setKbs(res.knowledge_bases);
      const ids = res.knowledge_bases.map((kb) => kb.id);
      const current = getActiveKB();
      if (!ids.includes(current)) {
        const fallback = ids.includes("default") ? "default" : ids[0] ?? "default";
        setActiveKB(fallback);
        setKbID(fallback);
      }
      setKbError(null);
    } catch (e) {
      setKbError(e instanceof Error ? e.message : String(e));
    } finally {
      setKbLoading(false);
    }
  }, [canReadKB]);

  const loadPermissions = useCallback(async (id: string) => {
    try {
      const res = await listKnowledgeBasePermissions(id);
      setPerms(res.permissions);
    } catch {
      setPerms([]);
    }
  }, []);

  const currentKB = kbs.find((kb) => kb.id === kbID);
  const currentRole = currentKB?.role ?? "";

  useEffect(() => {
    if (currentRole !== "kb:admin") {
      setPerms([]);
      return;
    }
    void loadPermissions(kbID);
  }, [kbID, currentRole, loadPermissions]);

  useEffect(() => {
    if (!getToken() && !isAnonymous()) {
      navigate("/login", { replace: true });
      return;
    }
    setReady(true);
    getMe()
      .then(setMe)
      .catch(() => setMe(null));
    void loadKBs();
  }, [navigate, loadKBs]);

  if (!ready) return null;

  const nav = hasPermission("user:read") ? [...NAV, { to: "/users", label: "用户管理" }] : NAV;

  const handleLogout = () => {
    clearAuth();
    navigate("/login", { replace: true });
  };

  const switchKB = (id: string) => {
    setActiveKB(id);
    setKbID(id);
  };

  const handleCreate = async () => {
    if (!newName.trim() || busy) return;
    setBusy(true);
    setKbError(null);
    try {
      const res = await createKnowledgeBase({ name: newName.trim(), description: newDesc.trim() });
      setNewName("");
      setNewDesc("");
      await loadKBs();
      if (res.knowledge_base?.id) switchKB(res.knowledge_base.id);
    } catch (e) {
      setKbError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const handleUpdate = async (id: string) => {
    if (!editName.trim() || busy) return;
    setBusy(true);
    setKbError(null);
    try {
      await updateKnowledgeBase(id, { name: editName.trim(), description: editDesc.trim() });
      setEditID(null);
      await loadKBs();
    } catch (e) {
      setKbError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const handleDelete = async (kb: KnowledgeBase) => {
    if (busy) return;
    if (!window.confirm(`确定删除知识库「${kb.name}」吗？其中已上传的文件不会自动删除。`)) return;
    setBusy(true);
    setKbError(null);
    try {
      await deleteKnowledgeBase(kb.id);
      if (kbID === kb.id) switchKB("default");
      await loadKBs();
    } catch (e) {
      setKbError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const handleGrant = async () => {
    if (!currentKB || !permSubjectID.trim() || busy) return;
    setBusy(true);
    setKbError(null);
    try {
      await grantKnowledgeBasePermission(currentKB.id, {
        subject_type: permSubjectType,
        subject_id: permSubjectID.trim(),
        permission: permRole,
      });
      setPermSubjectID("");
      await loadPermissions(currentKB.id);
    } catch (e) {
      setKbError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const handleRevoke = async (perm: KnowledgeBasePermission) => {
    if (!currentKB || busy) return;
    setBusy(true);
    setKbError(null);
    try {
      await revokeKnowledgeBasePermission(currentKB.id, perm.subject_type, perm.subject_id);
      await loadPermissions(currentKB.id);
    } catch (e) {
      setKbError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const activeKB = currentKB;

  return (
    <div className="flex h-screen overflow-hidden">
      <aside className="flex w-56 shrink-0 flex-col border-r border-white/5 bg-ink-950/60">
        <div className="flex items-center gap-2.5 px-5 py-4">
          <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-accent/15 text-xs font-bold text-accent">
            N
          </div>
          <div>
            <p className="text-sm font-semibold text-slate-100">NexusFlow</p>
            <p className="text-[9px] uppercase tracking-wider text-slate-500">Console</p>
          </div>
        </div>
        <nav className="mt-2 flex flex-col gap-0.5 px-2">
          {nav.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) =>
                `flex items-center rounded-lg px-3 py-2 text-xs transition ${
                  isActive
                    ? "bg-accent/10 text-accent"
                    : "text-slate-400 hover:bg-white/5 hover:text-slate-200"
                }`
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto px-4 py-4 text-[9px] leading-relaxed text-slate-600">
          分布式文件向量化工作流 · V4.0
          <br />
          Go · Python · OpenSandbox
        </div>
      </aside>

      <div className="flex flex-1 flex-col overflow-hidden">
        <header className="flex h-14 shrink-0 items-center justify-between gap-3 border-b border-white/5 px-6">
          <div className="flex min-w-0 items-center gap-3">
            <p className="truncate text-[10px] uppercase tracking-[0.2em] text-slate-500">
              {user ? `${user.tenant_id} · ${user.username}` : me ? `${me.tenant_id} · ${me.username ?? me.user_id}` : "未连接"}
            </p>
            {canReadKB && (
              <div className="flex items-center gap-1.5">
                <span className="text-[10px] text-slate-500">知识库</span>
                <select
                  value={kbID}
                  onChange={(e) => switchKB(e.target.value)}
                  title={activeKB?.description || undefined}
                  className="max-w-40 rounded-lg border border-white/10 bg-ink-950 px-2 py-1 text-[11px] text-slate-200 outline-none focus:border-accent/40"
                >
                  {kbs.length === 0 && <option value="default">默认知识库</option>}
                  {kbs.map((kb) => (
                    <option key={kb.id} value={kb.id}>
                      {kb.name}
                    </option>
                  ))}
                </select>
                {activeKB?.role && (
                  <span className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                    {KB_ROLE_LABELS[activeKB.role] ?? activeKB.role}
                  </span>
                )}
                {(canWriteKB || activeKB?.role === "kb:admin") && (
                  <Button
                    variant="ghost"
                    className="px-2 py-1 text-[10px]"
                    onClick={() => setManaging(true)}
                  >
                    管理
                  </Button>
                )}
                {kbLoading && <Spinner className="h-3 w-3" />}
              </div>
            )}
          </div>
          <div className="flex items-center gap-2">
            <span
              className={`flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[10px] ${
                !anon && getToken()
                  ? "bg-emerald-500/10 text-emerald-300"
                  : me
                    ? "bg-sky-500/10 text-sky-300"
                    : "bg-rose-500/10 text-rose-300"
              }`}
            >
              <span className="h-1.5 w-1.5 rounded-full bg-current" />
              {getToken() ? "已鉴权" : me ? "匿名访问" : "离线"}
            </span>
            {anon && !getToken() ? (
              <Button variant="ghost" className="px-2.5 py-1 text-[10px]" onClick={() => navigate("/login")}>
                登录
              </Button>
            ) : (
              <Button variant="ghost" className="px-2.5 py-1 text-[10px]" onClick={handleLogout}>
                登出
              </Button>
            )}
          </div>
        </header>

        <main className="flex-1 overflow-auto p-6">
          <Outlet context={{ kbID, kbRole: currentRole } satisfies LayoutContext} />
        </main>
      </div>

      {managing && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
          onClick={() => setManaging(false)}
        >
          <div
            className="panel flex max-h-[80vh] w-full max-w-lg flex-col p-5"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="mb-4 flex items-center justify-between">
              <h3 className="panel-title">知识库管理</h3>
              <button
                type="button"
                onClick={() => setManaging(false)}
                className="rounded px-2 py-0.5 text-xs text-slate-400 hover:bg-white/5 hover:text-slate-200"
              >
                关闭
              </button>
            </div>

            <div className="mb-4 space-y-2 rounded-lg border border-white/5 bg-white/[0.02] p-3">
              <p className="text-[10px] uppercase tracking-wider text-slate-500">新建知识库</p>
              <input
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="名称，例如：研发资料"
                className="w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-1.5 text-xs text-slate-200 outline-none focus:border-accent/40"
              />
              <input
                value={newDesc}
                onChange={(e) => setNewDesc(e.target.value)}
                placeholder="描述（可选）"
                className="w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-1.5 text-xs text-slate-200 outline-none focus:border-accent/40"
              />
              <Button disabled={busy || !newName.trim()} onClick={() => void handleCreate()}>
                创建
              </Button>
            </div>

            <div className="min-h-0 flex-1 space-y-2 overflow-auto">
              {kbs.map((kb) => (
                <div key={kb.id} className="rounded-lg border border-white/5 p-3">
                  {editID === kb.id ? (
                    <div className="space-y-2">
                      <input
                        value={editName}
                        onChange={(e) => setEditName(e.target.value)}
                        className="w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-1.5 text-xs text-slate-200 outline-none focus:border-accent/40"
                      />
                      <input
                        value={editDesc}
                        onChange={(e) => setEditDesc(e.target.value)}
                        placeholder="描述（可选）"
                        className="w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-1.5 text-xs text-slate-200 outline-none focus:border-accent/40"
                      />
                      <div className="flex gap-2">
                        <Button disabled={busy || !editName.trim()} onClick={() => void handleUpdate(kb.id)}>
                          保存
                        </Button>
                        <Button variant="ghost" onClick={() => setEditID(null)}>
                          取消
                        </Button>
                      </div>
                    </div>
                  ) : (
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <p className="flex items-center gap-1.5 text-xs text-slate-200">
                          {kb.name}
                          {kb.id === kbID && (
                            <span className="rounded bg-accent/10 px-1.5 py-0.5 text-[9px] text-accent">当前</span>
                          )}
                          {kb.id === "default" && (
                            <span className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-500">默认</span>
                          )}
                        </p>
                        <p className="mt-0.5 break-words text-[10px] text-slate-500">
                          {kb.description || "无描述"}
                        </p>
                      </div>
                      <div className="flex shrink-0 items-center gap-1.5">
                        {kb.role && (
                          <span className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                            {KB_ROLE_LABELS[kb.role] ?? kb.role}
                          </span>
                        )}
                        {kb.role === "kb:admin" && (
                          <>
                            <Button
                              variant="ghost"
                              className="px-2 py-1 text-[10px]"
                              disabled={busy}
                              onClick={() => {
                                setEditID(kb.id);
                                setEditName(kb.name);
                                setEditDesc(kb.description);
                              }}
                            >
                              重命名
                            </Button>
                            {kb.id !== "default" && (
                              <Button
                                variant="danger"
                                className="px-2 py-1 text-[10px]"
                                disabled={busy}
                                onClick={() => void handleDelete(kb)}
                              >
                                删除
                              </Button>
                            )}
                          </>
                        )}
                      </div>
                    </div>
                  )}
                </div>
              ))}
            </div>

            {currentKB?.role === "kb:admin" && (
              <div className="mt-3 rounded-lg border border-white/5 bg-white/[0.02] p-3">
                <p className="mb-2 text-[10px] uppercase tracking-wider text-slate-500">
                  成员授权 · {currentKB.name}
                </p>
                <div className="flex flex-wrap items-center gap-2">
                  <select
                    value={permSubjectType}
                    onChange={(e) => setPermSubjectType(e.target.value)}
                    className="rounded-lg border border-white/10 bg-ink-950 px-2 py-1 text-[11px] text-slate-200 outline-none focus:border-accent/40"
                  >
                    <option value="user">用户</option>
                    <option value="role">角色</option>
                  </select>
                  <input
                    value={permSubjectID}
                    onChange={(e) => setPermSubjectID(e.target.value)}
                    placeholder={permSubjectType === "user" ? "用户 ID" : "角色名，如 editor"}
                    className="min-w-0 flex-1 rounded-lg border border-white/10 bg-ink-950 px-2 py-1 text-[11px] text-slate-200 outline-none focus:border-accent/40"
                  />
                  <select
                    value={permRole}
                    onChange={(e) => setPermRole(e.target.value)}
                    className="rounded-lg border border-white/10 bg-ink-950 px-2 py-1 text-[11px] text-slate-200 outline-none focus:border-accent/40"
                  >
                    <option value="kb:viewer">只读</option>
                    <option value="kb:editor">编辑者</option>
                    <option value="kb:admin">管理员</option>
                  </select>
                  <Button
                    disabled={busy || !permSubjectID.trim()}
                    onClick={() => void handleGrant()}
                  >
                    授权
                  </Button>
                </div>
                {perms.length === 0 ? (
                  <p className="mt-2 text-[10px] text-slate-500">暂无额外授权</p>
                ) : (
                  <ul className="mt-2 space-y-1">
                    {perms.map((perm) => (
                      <li
                        key={perm.id}
                        className="flex items-center justify-between gap-2 rounded bg-white/[0.02] px-2 py-1 text-[10px] text-slate-300"
                      >
                        <span className="min-w-0 truncate">
                          {perm.subject_type === "user" ? "用户" : "角色"} · {perm.subject_id} ·{" "}
                          {KB_ROLE_LABELS[perm.permission] ?? perm.permission}
                        </span>
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() => void handleRevoke(perm)}
                          className="shrink-0 text-rose-300 hover:underline disabled:opacity-40"
                        >
                          移除
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )}

            {kbError && <p className="mt-3 text-xs text-rose-300">{kbError}</p>}
          </div>
        </div>
      )}
    </div>
  );
}
