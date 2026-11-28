import { useCallback, useEffect, useMemo, useState } from "react";
import {
  callSubAgent,
  createSubAgent,
  deleteSubAgent,
  hasPermission,
  listSkills,
  listSubAgents,
} from "../api/client";
import type { SkillInfo, SubAgentInput, SubAgentRecord } from "../types";
import { Button, Card, EmptyState, PageHeader, Spinner, Tag } from "../components/ui";

const inputClass =
  "w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40";

type Kind = "http" | "sandbox";

interface FormState {
  name: string;
  kind: Kind;
  capabilities: string;
  skills: string;
  endpoint: string;
  priority: string;
  timeout_ms: string;
  runtime: string;
  timeout_seconds: string;
  packages: string;
  code: string;
}

const EMPTY_FORM: FormState = {
  name: "",
  kind: "http",
  capabilities: "agent.call",
  skills: "",
  endpoint: "",
  priority: "100",
  timeout_ms: "5000",
  runtime: "python",
  timeout_seconds: "10",
  packages: "",
  code: "",
};

function errMsg(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

const HTTP_EXAMPLE = `{
  "name": "expense.agent",
  "kind": "http",
  "capabilities": ["agent.call"],
  "endpoint": "http://my-agent:9000/v1/call",
  "skills": ["expense.approval"],
  "priority": 100,
  "timeout_ms": 5000,
  "risk": "read"
}`;

const SANDBOX_EXAMPLE = `{
  "name": "chart.agent",
  "kind": "sandbox",
  "capabilities": ["agent.call"],
  "skills": ["chart.render"],
  "priority": 120,
  "risk": "execute",
  "sandbox": {
    "runtime": "python",
    "timeout_seconds": 10,
    "packages": ["numpy"],
    "code": "import json, sys\\ninp = json.load(sys.stdin)\\nprint(json.dumps({'echo': inp}))"
  }
}`;

function splitCSV(value: string): string[] {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

export default function SubAgents() {
  const canWrite = hasPermission("user:write");
  const [agents, setAgents] = useState<SubAgentRecord[]>([]);
  const [skills, setSkills] = useState<SkillInfo[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [form, setForm] = useState<FormState>(EMPTY_FORM);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [testCapability, setTestCapability] = useState("agent.call");
  const [testInput, setTestInput] = useState('{"message": "hello"}');
  const [testResult, setTestResult] = useState<string>("");
  const [testing, setTesting] = useState(false);

  const current = useMemo(
    () => agents.find((agent) => agent.name === selected && agent.tenant_id) ?? null,
    [agents, selected],
  );
  const builtins = useMemo(
    () => agents.filter((agent) => !agent.tenant_id || agent.kind === "local"),
    [agents],
  );
  const customAgents = useMemo(
    () => agents.filter((agent) => agent.tenant_id && agent.kind !== "local"),
    [agents],
  );

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [agentRes, skillRes] = await Promise.all([listSubAgents(), listSkills()]);
      // Empty repeated fields arrive as JSON null; normalize before rendering.
      setAgents(
        (agentRes.agents ?? []).map((agent) => ({
          ...agent,
          capabilities: agent.capabilities ?? [],
          skills: agent.skills ?? [],
        })),
      );
      setSkills(skillRes.skills ?? []);
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const flash = (message: string) => {
    setNotice(message);
    window.setTimeout(() => setNotice(null), 2500);
  };

  const pick = (agent: SubAgentRecord) => {
    setSelected(agent.name);
    setForm({
      name: agent.name,
      kind: agent.kind === "sandbox" ? "sandbox" : "http",
      capabilities: agent.capabilities.join(", "),
      skills: agent.skills.join(", "),
      endpoint: agent.endpoint,
      priority: String(agent.priority),
      timeout_ms: "5000",
      runtime: agent.sandbox?.runtime ?? "python",
      timeout_seconds: String(agent.sandbox?.timeout_seconds ?? 10),
      packages: (agent.sandbox?.packages ?? []).join(", "),
      code: "",
    });
    setTestCapability(agent.capabilities[0] ?? "agent.call");
    setTestResult("");
  };

  const reset = () => {
    setSelected(null);
    setForm(EMPTY_FORM);
    setTestResult("");
  };

  const save = async () => {
    if (!canWrite || busy) return;
    const capabilities = splitCSV(form.capabilities);
    if (!form.name.trim() || capabilities.length === 0) {
      setError("name 与 capabilities 必填");
      return;
    }
    const input: SubAgentInput = {
      name: form.name.trim(),
      kind: form.kind,
      capabilities,
      skills: splitCSV(form.skills),
      priority: Number(form.priority) || 100,
      timeout_ms: Number(form.timeout_ms) || 5000,
      risk: form.kind === "sandbox" ? "execute" : "read",
    };
    if (form.kind === "http") {
      input.endpoint = form.endpoint.trim();
    } else {
      input.sandbox = {
        runtime: form.runtime,
        timeout_seconds: Number(form.timeout_seconds) || 10,
        packages: splitCSV(form.packages),
      };
      if (form.code.trim()) input.sandbox.code = form.code;
    }
    setBusy(true);
    setError(null);
    try {
      await createSubAgent(input);
      flash(`已保存 ${input.name}`);
      await reload();
      setSelected(input.name);
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!canWrite || !current || busy) return;
    if (!window.confirm(`确定删除 SubAgent「${current.name}」吗？`)) return;
    setBusy(true);
    setError(null);
    try {
      await deleteSubAgent(current.name);
      flash(`已删除 ${current.name}`);
      reset();
      await reload();
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setBusy(false);
    }
  };

  const runTest = async () => {
    if (!current || testing) return;
    let payload: Record<string, unknown>;
    try {
      const parsed = JSON.parse(testInput) as unknown;
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
        throw new Error("input 必须是 JSON 对象");
      }
      payload = parsed as Record<string, unknown>;
    } catch (error_) {
      setError(errMsg(error_));
      return;
    }
    setTesting(true);
    setError(null);
    setTestResult("");
    try {
      const result = await callSubAgent(current.name, testCapability, payload);
      setTestResult(JSON.stringify(result, null, 2));
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setTesting(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="SubAgent 管理"
        description="内置 Agent 只读展示；按标准注册 HTTP / Sandbox Agent，供动态技能路由与在线调试"
        actions={
          <>
            <Button variant="ghost" onClick={() => void reload()} disabled={loading || busy}>
              刷新
            </Button>
            <Button variant="subtle" onClick={reset} disabled={busy}>
              新建
            </Button>
          </>
        }
      />

      {!canWrite && (
        <div className="panel mb-4 p-3 text-xs text-amber-300">
          当前账号只有查看权限；保存与删除需要 user:write。
        </div>
      )}
      {error && <div className="panel mb-4 p-3 text-xs text-rose-300">{error}</div>}
      {notice && <div className="panel mb-4 p-3 text-xs text-emerald-300">{notice}</div>}

      <div className="grid gap-4 xl:grid-cols-[1fr,1.1fr]">
        <div className="space-y-4">
          <Card title={`内置 Agent · ${builtins.length}`}>
            <p className="mb-3 text-[10px] leading-relaxed text-slate-500">
              由 agent-hub 启动时注册，供意图识别、摘要与记忆链路使用；只读，不可编辑或删除。
            </p>
            {builtins.length === 0 ? (
              <p className="text-xs text-slate-500">agent-hub 未开启内置 Agent。</p>
            ) : (
              <ul className="space-y-2">
                {builtins.map((agent) => (
                  <li
                    key={`builtin/${agent.name}`}
                    className="rounded-lg border border-white/5 bg-white/[0.02] p-3"
                  >
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-xs font-medium text-slate-200">{agent.name}</span>
                      <Tag>{agent.kind}</Tag>
                      <span className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400">
                        内置 · 只读
                      </span>
                      <span className="ml-auto text-[9px] text-slate-500">
                        risk {agent.risk} · priority {agent.priority}
                      </span>
                    </div>
                    <div className="mt-2 flex flex-wrap gap-1">
                      {agent.capabilities.map((capability) => (
                        <span
                          key={capability}
                          className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400"
                        >
                          {capability}
                        </span>
                      ))}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card
            title={`自定义 SubAgent · ${customAgents.length}`}
            action={loading ? <Spinner className="h-3 w-3" /> : undefined}
          >
            {customAgents.length === 0 && !loading ? (
              <EmptyState
                title="暂无自定义 SubAgent"
                hint="点击“新建”注册 HTTP Agent 或 Sandbox Agent；内置 Agent 不在此列表。"
              />
            ) : (
              <ul className="space-y-2">
                {customAgents.map((agent) => (
                  <li
                    key={`${agent.tenant_id}/${agent.name}`}
                    className={`cursor-pointer rounded-lg border p-3 transition ${
                      selected === agent.name
                        ? "border-accent/40 bg-accent/5"
                        : "border-white/5 bg-white/[0.02] hover:border-white/10"
                    }`}
                    onClick={() => pick(agent)}
                  >
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-xs font-medium text-slate-200">{agent.name}</span>
                      <Tag>{agent.kind}</Tag>
                      <span
                        className={`rounded px-1.5 py-0.5 text-[9px] ${
                          agent.healthy
                            ? "bg-emerald-500/10 text-emerald-300"
                            : "bg-rose-500/10 text-rose-300"
                        }`}
                      >
                        {agent.healthy ? "healthy" : "unhealthy"}
                      </span>
                      <span className="ml-auto text-[9px] text-slate-500">
                        priority {agent.priority}
                      </span>
                    </div>
                    <div className="mt-2 flex flex-wrap gap-1">
                      {agent.capabilities.map((capability) => (
                        <span
                          key={capability}
                          className="rounded bg-white/5 px-1.5 py-0.5 text-[9px] text-slate-400"
                        >
                          {capability}
                        </span>
                      ))}
                      {agent.skills.map((skill) => (
                        <span
                          key={skill}
                          className="rounded bg-accent/10 px-1.5 py-0.5 text-[9px] text-accent"
                        >
                          {skill}
                        </span>
                      ))}
                    </div>
                    <p className="mt-1.5 truncate text-[10px] text-slate-500">
                      {agent.kind === "http"
                        ? agent.endpoint
                        : `sandbox · ${agent.sandbox?.runtime ?? "python"} · ${
                            agent.sandbox?.timeout_seconds ?? 10
                          }s · ${agent.sandbox?.code_bytes ?? 0}B`}
                    </p>
                  </li>
                ))}
            </ul>
          )}
          </Card>
        </div>

        <div className="space-y-4">
          <Card
            title={selected ? `编辑 · ${selected}` : "新建 SubAgent"}
            action={
              selected && canWrite ? (
                <Button
                  variant="danger"
                  className="px-2 py-1 text-[10px]"
                  onClick={() => void remove()}
                  disabled={busy}
                >
                  删除
                </Button>
              ) : undefined
            }
          >
            <div className="space-y-3">
              <div className="grid gap-3 sm:grid-cols-2">
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">名称</span>
                  <input
                    value={form.name}
                    onChange={(event) => setForm({ ...form, name: event.target.value })}
                    placeholder="expense.agent"
                    disabled={Boolean(selected) || !canWrite}
                    className={inputClass}
                  />
                </label>
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">类型</span>
                  <select
                    value={form.kind}
                    onChange={(event) =>
                      setForm({ ...form, kind: event.target.value as Kind })
                    }
                    disabled={!canWrite}
                    className={inputClass}
                  >
                    <option value="http">HTTP Agent</option>
                    <option value="sandbox">Sandbox Agent</option>
                  </select>
                </label>
              </div>

              <label className="block">
                <span className="mb-1 block text-[10px] text-slate-500">
                  capabilities（逗号分隔，如 agent.call）
                </span>
                <input
                  value={form.capabilities}
                  onChange={(event) => setForm({ ...form, capabilities: event.target.value })}
                  disabled={!canWrite}
                  className={inputClass}
                />
              </label>
              <label className="block">
                <span className="mb-1 block text-[10px] text-slate-500">
                  skills（逗号分隔，如 expense.approval）
                </span>
                <input
                  value={form.skills}
                  onChange={(event) => setForm({ ...form, skills: event.target.value })}
                  disabled={!canWrite}
                  className={inputClass}
                />
              </label>

              {form.kind === "http" ? (
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">endpoint</span>
                  <input
                    value={form.endpoint}
                    onChange={(event) => setForm({ ...form, endpoint: event.target.value })}
                    placeholder="http://my-agent:9000/v1/call"
                    disabled={!canWrite}
                    className={inputClass}
                  />
                </label>
              ) : (
                <>
                  <div className="grid gap-3 sm:grid-cols-3">
                    <label className="block">
                      <span className="mb-1 block text-[10px] text-slate-500">runtime</span>
                      <select
                        value={form.runtime}
                        onChange={(event) => setForm({ ...form, runtime: event.target.value })}
                        disabled={!canWrite}
                        className={inputClass}
                      >
                        <option value="python">python</option>
                        <option value="node">node</option>
                      </select>
                    </label>
                    <label className="block">
                      <span className="mb-1 block text-[10px] text-slate-500">超时（秒）</span>
                      <input
                        value={form.timeout_seconds}
                        onChange={(event) =>
                          setForm({ ...form, timeout_seconds: event.target.value })
                        }
                        disabled={!canWrite}
                        className={inputClass}
                      />
                    </label>
                    <label className="block">
                      <span className="mb-1 block text-[10px] text-slate-500">packages</span>
                      <input
                        value={form.packages}
                        onChange={(event) => setForm({ ...form, packages: event.target.value })}
                        placeholder="numpy, pandas"
                        disabled={!canWrite}
                        className={inputClass}
                      />
                    </label>
                  </div>
                  <label className="block">
                    <span className="mb-1 block text-[10px] text-slate-500">
                      代码{current?.sandbox?.configured ? "（留空保留现有代码）" : ""}
                    </span>
                    <textarea
                      value={form.code}
                      onChange={(event) => setForm({ ...form, code: event.target.value })}
                      rows={7}
                      placeholder="import json&#10;print(json.dumps({'ok': True}))"
                      disabled={!canWrite}
                      className={`${inputClass} font-mono`}
                    />
                  </label>
                </>
              )}

              <div className="grid gap-3 sm:grid-cols-2">
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">priority</span>
                  <input
                    value={form.priority}
                    onChange={(event) => setForm({ ...form, priority: event.target.value })}
                    disabled={!canWrite}
                    className={inputClass}
                  />
                </label>
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">timeout_ms</span>
                  <input
                    value={form.timeout_ms}
                    onChange={(event) => setForm({ ...form, timeout_ms: event.target.value })}
                    disabled={!canWrite}
                    className={inputClass}
                  />
                </label>
              </div>

              <div className="flex items-center gap-2">
                <Button
                  onClick={() => void save()}
                  disabled={!canWrite || busy || !form.name.trim()}
                >
                  {busy ? "保存中…" : "保存"}
                </Button>
                {selected && (
                  <Button variant="ghost" onClick={reset} disabled={busy}>
                    取消选择
                  </Button>
                )}
                <span className="text-[10px] text-slate-500">
                  保存受 AGENT_HUB_DYNAMIC_REGISTRATION 控制
                </span>
              </div>
            </div>
          </Card>

          <Card title="在线调试">
            {!current ? (
              <p className="text-xs text-slate-500">在左侧选择一个 SubAgent。</p>
            ) : (
              <div className="space-y-3">
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">capability</span>
                  <select
                    value={testCapability}
                    onChange={(event) => setTestCapability(event.target.value)}
                    className={inputClass}
                  >
                    {current.capabilities.map((capability) => (
                      <option key={capability} value={capability}>
                        {capability}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="block">
                  <span className="mb-1 block text-[10px] text-slate-500">input（JSON）</span>
                  <textarea
                    value={testInput}
                    onChange={(event) => setTestInput(event.target.value)}
                    rows={5}
                    className={`${inputClass} font-mono`}
                  />
                </label>
                <Button onClick={() => void runTest()} disabled={testing}>
                  {testing ? "调用中…" : "调用"}
                </Button>
                {testResult && (
                  <pre className="max-h-64 overflow-auto rounded-lg border border-white/5 bg-ink-950 p-3 text-[10px] text-slate-300">
                    {testResult}
                  </pre>
                )}
              </div>
            )}
          </Card>

          <Card title={`技能 · ${skills.length}`}>
            {skills.length === 0 ? (
              <p className="text-xs text-slate-500">当前租户暂无技能；在 Agent 的 skills 中声明后即可被路由。</p>
            ) : (
              <ul className="space-y-1.5">
                {skills.map((skill) => (
                  <li
                    key={skill.skill}
                    className="flex items-center justify-between gap-2 rounded bg-white/[0.02] px-2 py-1 text-[10px]"
                  >
                    <span className="text-accent">{skill.skill}</span>
                    <span className="text-slate-500">
                      → {skill.agent} · priority {skill.priority}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card title="创建标准与示例">
            <ol className="list-decimal space-y-1 pl-4 text-[10px] leading-relaxed text-slate-400">
              <li>
                命名使用小写字母、数字和点（如 <code>expense.agent</code>），同租户内唯一；
                <code>local.*</code> 为内置保留前缀。
              </li>
              <li>
                <code>capabilities</code> 至少一个，需与调用方声明一致（如
                <code>agent.call</code>）；<code>skills</code> 决定动态技能路由，priority 越小越优先。
              </li>
              <li>
                HTTP Agent：<code>endpoint</code> 必须是可访问的 POST JSON 地址，返回{" "}
                <code>{'{"ok":true,"data":{}}'}</code>，默认 5s 超时。
              </li>
              <li>
                Sandbox Agent：runtime 支持 python / node，超时不超过 60s，代码从 stdin 读取
                JSON、向 stdout 输出 JSON。
              </li>
              <li>
                写入受 <code>AGENT_HUB_DYNAMIC_REGISTRATION</code> 控制；内置{" "}
                <code>local.*</code> 只读，不能覆盖或删除。
              </li>
            </ol>
            <div className="mt-3 space-y-2">
              <p className="text-[10px] uppercase tracking-wider text-slate-500">HTTP 示例</p>
              <pre className="max-h-52 overflow-auto rounded-lg border border-white/5 bg-ink-950 p-3 text-[10px] leading-relaxed text-slate-300">
                {HTTP_EXAMPLE}
              </pre>
              <p className="text-[10px] uppercase tracking-wider text-slate-500">Sandbox 示例</p>
              <pre className="max-h-56 overflow-auto rounded-lg border border-white/5 bg-ink-950 p-3 text-[10px] leading-relaxed text-slate-300">
                {SANDBOX_EXAMPLE}
              </pre>
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}
