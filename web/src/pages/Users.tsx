import { useCallback, useEffect, useState } from "react";
import {
  createUser,
  deleteRole,
  deleteUser,
  getCurrentUser,
  hasPermission,
  listPermissions,
  listRoles,
  listUsers,
  updateUser,
  upsertRole,
} from "../api/client";
import type { AuthUser, PermissionInfo, RoleInfo } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner, Tag } from "../components/ui";

const inputClass =
  "w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40";

function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export default function Users() {
  const me = getCurrentUser();
  const canWrite = hasPermission("user:write");
  const canDelete = hasPermission("user:delete");
  const canWriteRole = hasPermission("role:write");

  const [tab, setTab] = useState<"users" | "roles">("users");
  const [users, setUsers] = useState<AuthUser[]>([]);
  const [roles, setRoles] = useState<RoleInfo[]>([]);
  const [perms, setPerms] = useState<PermissionInfo[]>([]);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const reload = useCallback(async (q: string) => {
    setBusy(true);
    setError(null);
    try {
      const [u, r, p] = await Promise.all([listUsers(q), listRoles(), listPermissions()]);
      setUsers(u.users);
      setRoles(r.roles);
      setPerms(p.permissions);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => {
    void reload("");
  }, [reload]);

  const flash = (msg: string) => {
    setNotice(msg);
    window.setTimeout(() => setNotice(null), 2500);
  };

  // ----- create user form -----
  const [uForm, setUForm] = useState({ username: "", password: "", email: "", role: "viewer" });
  const setUserRole = (name: string) => setUForm((f) => ({ ...f, role: name }));

  const submitCreateUser = async () => {
    if (!uForm.username.trim() || !uForm.password) return;
    try {
      await createUser({
        username: uForm.username.trim(),
        password: uForm.password,
        email: uForm.email.trim() || undefined,
        roles: [uForm.role],
      });
      setUForm({ username: "", password: "", email: "", role: "viewer" });
      flash("用户已创建");
      await reload(query);
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const toggleStatus = async (u: AuthUser) => {
    try {
      await updateUser(u.id, { status: u.status === "active" ? "disabled" : "active" });
      await reload(query);
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const removeUser = async (u: AuthUser) => {
    if (!window.confirm(`确认删除用户 ${u.username}？`)) return;
    try {
      await deleteUser(u.id);
      flash("用户已删除");
      await reload(query);
    } catch (e) {
      setError(errMsg(e));
    }
  };

  // ----- create role form -----
  const [rForm, setRForm] = useState({ name: "", description: "", permissions: [] as string[] });
  const togglePerm = (key: string) =>
    setRForm((f) => ({
      ...f,
      permissions: f.permissions.includes(key)
        ? f.permissions.filter((p) => p !== key)
        : [...f.permissions, key],
    }));

  const submitRole = async () => {
    if (!rForm.name.trim()) return;
    try {
      await upsertRole({
        name: rForm.name.trim(),
        description: rForm.description.trim() || undefined,
        permissions: rForm.permissions,
      });
      setRForm({ name: "", description: "", permissions: [] });
      flash("角色已保存");
      await reload(query);
    } catch (e) {
      setError(errMsg(e));
    }
  };

  const removeRole = async (r: RoleInfo) => {
    if (!window.confirm(`确认删除角色 ${r.name}？`)) return;
    try {
      await deleteRole(r.name);
      flash("角色已删除");
      await reload(query);
    } catch (e) {
      setError(errMsg(e));
    }
  };

  return (
    <div>
      <PageHeader title="用户与权限" description="按租户隔离账号，角色由权限点组合而成，权限随 JWT 下发" />

      <div className="mb-4 flex items-center gap-2">
        {(["users", "roles"] as const).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`rounded-lg px-3 py-1.5 text-xs transition ${
              tab === t ? "bg-accent/15 text-accent" : "text-slate-400 hover:bg-white/5 hover:text-slate-200"
            }`}
          >
            {t === "users" ? "用户" : "角色"}
          </button>
        ))}
        <div className="ml-auto flex items-center gap-2">
          {tab === "users" && (
            <>
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && reload(query)}
                placeholder="搜索用户名 / 邮箱"
                className={`${inputClass} w-56`}
              />
              <Button variant="ghost" onClick={() => reload(query)} disabled={busy}>
                刷新
              </Button>
            </>
          )}
        </div>
      </div>

      {error && <div className="panel mb-4 p-3 text-xs text-rose-300">{error}</div>}
      {notice && <div className="panel mb-4 p-3 text-xs text-emerald-300">{notice}</div>}

      {tab === "users" && (
        <div className="grid gap-4 lg:grid-cols-[1fr,320px]">
          <Card title={`用户列表 · ${users.length}`}>
            {busy && users.length === 0 ? (
              <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
                <Spinner /> 加载中…
              </div>
            ) : users.length === 0 ? (
              <EmptyState title="暂无用户" hint="在右侧创建账号，或使用自助注册。" />
            ) : (
              <ul className="space-y-2">
                {users.map((u) => (
                  <li
                    key={u.id}
                    className="flex flex-wrap items-center gap-2 rounded-lg border border-white/5 bg-white/[0.02] p-3"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="text-xs font-medium text-slate-200">{u.username}</span>
                        {me?.id === u.id && <Tag>我</Tag>}
                        <span
                          className={`rounded px-1.5 py-0.5 text-[9px] ${
                            u.status === "active"
                              ? "bg-emerald-500/10 text-emerald-300"
                              : "bg-rose-500/10 text-rose-300"
                          }`}
                        >
                          {u.status || "active"}
                        </span>
                      </div>
                      <div className="mt-1 flex flex-wrap items-center gap-1">
                        {u.email && <span className="text-[10px] text-slate-500">{u.email}</span>}
                        {u.roles.map((r) => (
                          <Tag key={r}>{r}</Tag>
                        ))}
                        {u.departments?.map((d) => (
                          <Tag key={d}>#{d}</Tag>
                        ))}
                      </div>
                    </div>
                    {canWrite && (
                      <Button variant="subtle" onClick={() => toggleStatus(u)} className="px-2 py-1 text-[10px]">
                        {u.status === "active" ? "禁用" : "启用"}
                      </Button>
                    )}
                    {canDelete && me?.id !== u.id && (
                      <Button variant="danger" onClick={() => removeUser(u)} className="px-2 py-1 text-[10px]">
                        删除
                      </Button>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card title="创建用户">
            {!canWrite ? (
              <p className="text-xs text-slate-500">当前账号没有 user:write 权限。</p>
            ) : (
              <div className="space-y-3">
                <input
                  value={uForm.username}
                  onChange={(e) => setUForm({ ...uForm, username: e.target.value })}
                  placeholder="用户名"
                  className={inputClass}
                />
                <input
                  type="password"
                  value={uForm.password}
                  onChange={(e) => setUForm({ ...uForm, password: e.target.value })}
                  placeholder="初始密码"
                  className={inputClass}
                />
                <input
                  value={uForm.email}
                  onChange={(e) => setUForm({ ...uForm, email: e.target.value })}
                  placeholder="邮箱（可选）"
                  className={inputClass}
                />
                <div>
                  <p className="mb-1 text-[10px] text-slate-500">
                    角色（单选，互斥）
                  </p>
                  <div className="flex flex-wrap gap-1.5">
                    {roles.map((r) => (
                      <button
                        key={r.name}
                        type="button"
                        onClick={() => setUserRole(r.name)}
                        title={r.description || undefined}
                        className={`rounded-md border px-2 py-1 text-[10px] transition ${
                          uForm.role === r.name
                            ? "border-accent/40 bg-accent/10 text-accent"
                            : "border-white/10 text-slate-400 hover:text-slate-200"
                        }`}
                      >
                        {r.name}
                      </button>
                    ))}
                  </div>
                  <p className="mt-1 text-[10px] text-slate-600">
                    一个账号只承载一个角色，权限由该角色的权限点决定。
                  </p>
                </div>
                <Button
                  onClick={submitCreateUser}
                  disabled={!uForm.username.trim() || !uForm.password}
                  className="w-full"
                >
                  创建
                </Button>
              </div>
            )}
          </Card>
        </div>
      )}

      {tab === "roles" && (
        <div className="grid gap-4 lg:grid-cols-[1fr,360px]">
          <Card title={`角色 · ${roles.length}`}>
            <ul className="space-y-2">
              {roles.map((r) => (
                <li key={r.name} className="rounded-lg border border-white/5 bg-white/[0.02] p-3">
                  <div className="flex items-center gap-2">
                    <span className="text-xs font-medium text-slate-200">{r.name}</span>
                    {r.builtin && <Tag>内置</Tag>}
                    {r.description && <span className="text-[10px] text-slate-500">{r.description}</span>}
                    {canWriteRole && !r.builtin && (
                      <Button
                        variant="danger"
                        onClick={() => removeRole(r)}
                        className="ml-auto px-2 py-1 text-[10px]"
                      >
                        删除
                      </Button>
                    )}
                  </div>
                  <div className="mt-1.5 flex flex-wrap gap-1">
                    {r.permissions.map((p) => (
                      <span key={p} className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                        {p === "*" ? "全部权限" : p}
                      </span>
                    ))}
                  </div>
                </li>
              ))}
            </ul>
          </Card>

          <Card title="新建 / 更新角色">
            {!canWriteRole ? (
              <p className="text-xs text-slate-500">当前账号没有 role:write 权限。</p>
            ) : (
              <div className="space-y-3">
                <input
                  value={rForm.name}
                  onChange={(e) => setRForm({ ...rForm, name: e.target.value })}
                  placeholder="角色名（如 auditor）"
                  className={inputClass}
                />
                <input
                  value={rForm.description}
                  onChange={(e) => setRForm({ ...rForm, description: e.target.value })}
                  placeholder="描述（可选）"
                  className={inputClass}
                />
                <div>
                  <p className="mb-1 text-[10px] text-slate-500">权限点（内置角色不可覆盖）</p>
                  <div className="max-h-64 space-y-1 overflow-auto pr-1">
                    {perms
                      .filter((p) => p.key !== "*")
                      .map((p) => (
                        <label
                          key={p.key}
                          className="flex cursor-pointer items-center gap-2 rounded px-1 py-0.5 text-[10px] text-slate-400 hover:bg-white/5"
                        >
                          <input
                            type="checkbox"
                            checked={rForm.permissions.includes(p.key)}
                            onChange={() => togglePerm(p.key)}
                            className="accent-accent"
                          />
                          <span className="font-mono text-slate-300">{p.key}</span>
                          <span className="text-slate-600">{p.description}</span>
                        </label>
                      ))}
                  </div>
                </div>
                <Button onClick={submitRole} disabled={!rForm.name.trim()} className="w-full">
                  保存角色
                </Button>
              </div>
            )}
          </Card>
        </div>
      )}
    </div>
  );
}
