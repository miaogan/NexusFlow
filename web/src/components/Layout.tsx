import { useEffect, useState } from "react";
import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { clearAuth, getCurrentUser, getMe, getToken, hasPermission, isAnonymous } from "../api/client";
import type { MeInfo } from "../types";
import { Button } from "./ui";

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

export default function Layout() {
  const navigate = useNavigate();
  const [me, setMe] = useState<MeInfo | null>(null);
  const [ready, setReady] = useState(false);
  const user = getCurrentUser();
  const anon = isAnonymous();

  useEffect(() => {
    if (!getToken() && !isAnonymous()) {
      navigate("/login", { replace: true });
      return;
    }
    setReady(true);
    getMe()
      .then(setMe)
      .catch(() => setMe(null));
  }, [navigate]);

  if (!ready) return null;

  const nav = hasPermission("user:read") ? [...NAV, { to: "/users", label: "用户管理" }] : NAV;

  const handleLogout = () => {
    clearAuth();
    navigate("/login", { replace: true });
  };

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
        <header className="flex h-14 shrink-0 items-center justify-between border-b border-white/5 px-6">
          <p className="text-[10px] uppercase tracking-[0.2em] text-slate-500">
            {user ? `${user.tenant_id} · ${user.username}` : me ? `${me.tenant_id} · ${me.username ?? me.user_id}` : "未连接"}
          </p>
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
          <Outlet />
        </main>
      </div>
    </div>
  );
}
