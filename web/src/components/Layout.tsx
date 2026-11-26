import { useEffect, useState } from "react";
import { NavLink, Outlet } from "react-router-dom";
import { getMe, getToken, setToken } from "../api/client";
import type { MeInfo } from "../types";
import { Button } from "./ui";

const NAV = [
  { to: "/", label: "概览", end: true },
  { to: "/files", label: "文件管理" },
  { to: "/tasks", label: "任务中心" },
  { to: "/search", label: "检索测试" },
  { to: "/workflow", label: "工作流编排" },
  { to: "/sandbox", label: "沙箱 Playground" },
];

export default function Layout() {
  const [me, setMe] = useState<MeInfo | null>(null);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [tokenDraft, setTokenDraft] = useState("");

  useEffect(() => {
    getMe()
      .then(setMe)
      .catch(() => setMe(null));
  }, []);

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
          {NAV.map((item) => (
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
            {me ? `${me.tenant_id} · ${me.user_id}` : "未连接"}
          </p>
          <div className="flex items-center gap-2">
            <span
              className={`flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[10px] ${
                me ? "bg-emerald-500/10 text-emerald-300" : "bg-rose-500/10 text-rose-300"
              }`}
            >
              <span className="h-1.5 w-1.5 rounded-full bg-current" />
              {getToken() ? "已鉴权" : me ? "匿名访问" : "离线"}
            </span>
            <Button variant="ghost" className="px-2.5 py-1 text-[10px]" onClick={() => {
              setTokenDraft(getToken() ?? "");
              setTokenOpen(true);
            }}>
              Token
            </Button>
          </div>
        </header>

        <main className="flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>

      {tokenOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={() => setTokenOpen(false)}>
          <div className="panel w-96 p-5" onClick={(e) => e.stopPropagation()}>
            <h3 className="text-sm font-semibold text-slate-100">访问令牌</h3>
            <p className="mt-1 text-xs text-slate-500">
              留空则以匿名身份访问（需服务端开启 ALLOW_ANONYMOUS）。粘贴 JWT 可切换租户身份。
            </p>
            <textarea
              value={tokenDraft}
              onChange={(e) => setTokenDraft(e.target.value)}
              placeholder="Bearer JWT..."
              className="mt-3 h-24 w-full resize-none rounded-lg border border-white/10 bg-ink-950 px-3 py-2 font-mono text-[11px] text-slate-200 outline-none focus:border-accent/40"
            />
            <div className="mt-4 flex justify-end gap-2">
              <Button variant="ghost" onClick={() => { setToken(null); setTokenOpen(false); location.reload(); }}>
                清除
              </Button>
              <Button onClick={() => { setToken(tokenDraft || null); setTokenOpen(false); location.reload(); }}>
                保存
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
