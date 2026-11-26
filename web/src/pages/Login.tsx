import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { login, register, setAnonymous, setAuth } from "../api/client";
import { Button, Card, Spinner } from "../components/ui";

const inputClass =
  "mt-1 w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-sm text-slate-200 outline-none focus:border-accent/40";

export default function Login() {
  const navigate = useNavigate();
  const [mode, setMode] = useState<"login" | "register">("login");
  const [tenant, setTenant] = useState("default");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const submit = async () => {
    if (!username.trim() || !password) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      if (mode === "login") {
        const res = await login(tenant.trim() || "default", username.trim(), password);
        setAuth(res);
        navigate("/", { replace: true });
      } else {
        await register(tenant.trim() || "default", username.trim(), password, email.trim() || undefined);
        setNotice("注册成功，请使用新账号登录。");
        setMode("login");
        setPassword("");
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-ink-950 px-4">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-2.5">
          <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-accent/15 text-sm font-bold text-accent">
            N
          </div>
          <div>
            <p className="text-sm font-semibold text-slate-100">NexusFlow</p>
            <p className="text-[9px] uppercase tracking-wider text-slate-500">Console</p>
          </div>
        </div>

        <Card title={mode === "login" ? "登录" : "注册新账号"}>
          <div className="space-y-3">
            <label className="block text-[10px] text-slate-500">
              租户
              <input value={tenant} onChange={(e) => setTenant(e.target.value)} className={inputClass} />
            </label>
            <label className="block text-[10px] text-slate-500">
              用户名
              <input
                value={username}
                autoFocus
                onChange={(e) => setUsername(e.target.value)}
                className={inputClass}
              />
            </label>
            <label className="block text-[10px] text-slate-500">
              密码
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && submit()}
                className={inputClass}
              />
            </label>
            {mode === "register" && (
              <label className="block text-[10px] text-slate-500">
                邮箱（可选）
                <input value={email} onChange={(e) => setEmail(e.target.value)} className={inputClass} />
              </label>
            )}

            {error && <p className="text-[11px] text-rose-300">登录失败：{error}</p>}
            {notice && <p className="text-[11px] text-emerald-300">{notice}</p>}

            <Button onClick={submit} disabled={busy || !username.trim() || !password} className="w-full">
              {busy ? <Spinner /> : mode === "login" ? "登录" : "注册"}
            </Button>

            <div className="flex items-center justify-between pt-1 text-[10px] text-slate-500">
              <button
                type="button"
                onClick={() => {
                  setMode(mode === "login" ? "register" : "login");
                  setError(null);
                  setNotice(null);
                }}
                className="text-accent hover:underline"
              >
                {mode === "login" ? "没有账号？注册" : "已有账号？登录"}
              </button>
              <button
                type="button"
                onClick={() => {
                  setAnonymous(true);
                  navigate("/");
                }}
                className="hover:text-slate-300"
              >
                以匿名身份浏览 →
              </button>
            </div>
          </div>
        </Card>

        <p className="mt-4 text-center text-[10px] leading-relaxed text-slate-600">
          默认管理员：<span className="font-mono text-slate-400">admin / admin123</span>
          <br />
          自助注册账号默认拥有 viewer 只读角色
        </p>
      </div>
    </div>
  );
}
