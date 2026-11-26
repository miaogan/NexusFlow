import { useState } from "react";
import { executeSandbox } from "../api/client";
import type { SandboxResult } from "../types";
import { Button, Card, PageHeader, Spinner } from "../components/ui";

const PYTHON_SAMPLE = `import json

# 输入位于 /tmp/input.json，结构化输出写入 /tmp/output.json
with open("/tmp/input.json") as f:
    data = json.load(f)

result = {
    "sum": data.get("a", 0) + data.get("b", 0),
    "doubled": data.get("a", 0) * 2,
}
with open("/tmp/output.json", "w") as f:
    json.dump(result, f)

print("computed:", result)
`;

const NODE_SAMPLE = `const fs = require("fs");

const data = JSON.parse(fs.readFileSync("/tmp/input.json", "utf8"));
const out = { echo: data, ts: Date.now() };
fs.writeFileSync("/tmp/output.json", JSON.stringify(out));
console.log("node:", out);
`;

const PRESETS: Record<string, { code: string; input: string }> = {
  python: { code: PYTHON_SAMPLE, input: JSON.stringify({ a: 3, b: 4 }, null, 2) },
  node: { code: NODE_SAMPLE, input: JSON.stringify({ a: 3, b: 4 }, null, 2) },
};

export default function Sandbox() {
  const [runtime, setRuntime] = useState<"python" | "node">("python");
  const [code, setCode] = useState(PYTHON_SAMPLE);
  const [input, setInput] = useState(PRESETS.python.input);
  const [timeout, setTimeoutSec] = useState(30);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<SandboxResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  const switchRuntime = (rt: "python" | "node") => {
    setRuntime(rt);
    setCode(PRESETS[rt].code);
    setInput(PRESETS[rt].input);
    setResult(null);
    setError(null);
  };

  const handleRun = async () => {
    setRunning(true);
    setError(null);
    setResult(null);
    try {
      const res = await executeSandbox({
        code,
        runtime,
        input_json: input.trim() || undefined,
        timeout_seconds: timeout,
      });
      setResult(res);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setRunning(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="沙箱 Playground"
        description="在隔离环境中执行自定义代码节点。输入通过 /tmp/input.json 注入，结构化输出回收到 /tmp/output.json"
      />

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card title="代码">
          <div className="mb-3 flex items-center gap-3">
            <div className="flex rounded-lg border border-white/10 p-0.5">
              {(["python", "node"] as const).map((rt) => (
                <button
                  key={rt}
                  onClick={() => switchRuntime(rt)}
                  className={`rounded-md px-3 py-1 text-[11px] transition ${
                    runtime === rt ? "bg-accent/15 text-accent" : "text-slate-400 hover:text-slate-200"
                  }`}
                >
                  {rt}
                </button>
              ))}
            </div>
            <label className="flex items-center gap-2 text-[11px] text-slate-500">
              超时
              <input
                type="number"
                min={1}
                max={300}
                value={timeout}
                onChange={(e) => setTimeoutSec(Number(e.target.value))}
                className="w-16 rounded border border-white/10 bg-ink-950 px-2 py-1 text-[11px] text-slate-200 outline-none focus:border-accent/40"
              />
              s
            </label>
            <Button onClick={handleRun} disabled={running} className="ml-auto">
              {running ? <><Spinner /> 执行中</> : "执行"}
            </Button>
          </div>

          <textarea
            value={code}
            onChange={(e) => setCode(e.target.value)}
            spellCheck={false}
            className="h-64 w-full resize-none rounded-lg border border-white/10 bg-ink-950 p-3 font-mono text-[12px] leading-relaxed text-slate-200 outline-none focus:border-accent/40"
          />

          <p className="mt-3 mb-1 text-[10px] uppercase tracking-wider text-slate-500">/tmp/input.json</p>
          <textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            spellCheck={false}
            className="h-24 w-full resize-none rounded-lg border border-white/10 bg-ink-950 p-3 font-mono text-[12px] text-slate-300 outline-none focus:border-accent/40"
          />
        </Card>

        <Card title="执行结果">
          {error && (
            <div className="rounded-lg border border-rose-500/20 bg-rose-500/5 p-3 text-xs text-rose-200">
              请求失败：{error}
            </div>
          )}
          {result && (
            <div className="space-y-3">
              <div className="flex items-center gap-2 text-[11px]">
                <span className={`rounded-full px-2 py-0.5 ${result.success ? "bg-emerald-500/10 text-emerald-300" : "bg-rose-500/10 text-rose-300"}`}>
                  {result.success ? "成功" : "失败"}
                </span>
                <span className="font-mono text-slate-500">{result.sandbox_id}</span>
              </div>

              {result.error_msg && (
                <pre className="overflow-auto rounded-lg border border-rose-500/20 bg-rose-500/5 p-3 text-[11px] text-rose-200">
                  {result.error_msg}
                </pre>
              )}
              {result.stdout && (
                <div>
                  <p className="mb-1 text-[10px] uppercase tracking-wider text-slate-500">stdout</p>
                  <pre className="overflow-auto rounded-lg bg-ink-950 p-3 font-mono text-[11px] text-emerald-200">{result.stdout}</pre>
                </div>
              )}
              {result.stderr && (
                <div>
                  <p className="mb-1 text-[10px] uppercase tracking-wider text-slate-500">stderr</p>
                  <pre className="overflow-auto rounded-lg bg-ink-950 p-3 font-mono text-[11px] text-amber-200">{result.stderr}</pre>
                </div>
              )}
              {result.output_json && (
                <div>
                  <p className="mb-1 text-[10px] uppercase tracking-wider text-slate-500">/tmp/output.json</p>
                  <pre className="overflow-auto rounded-lg border border-accent/20 bg-accent/5 p-3 font-mono text-[11px] text-accent-200">{result.output_json}</pre>
                </div>
              )}
            </div>
          )}
          {!result && !error && (
            <div className="flex h-full items-center justify-center py-20 text-center">
              <p className="text-xs text-slate-500">点击「执行」在沙箱中运行代码</p>
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
