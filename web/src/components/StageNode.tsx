import { Handle, Position, type NodeProps } from "reactflow";
import type { PipelineNodeData, Stage } from "../types";

const STAGE_STYLE: Record<Stage, { ring: string; badge: string; letter: string }> = {
  ingest: { ring: "border-sky-400/40", badge: "bg-sky-400/10 text-sky-300", letter: "摄" },
  parse: { ring: "border-violet-400/40", badge: "bg-violet-400/10 text-violet-300", letter: "解" },
  index: { ring: "border-accent/40", badge: "bg-accent/10 text-accent", letter: "索" },
  sandbox: { ring: "border-amber-400/40", badge: "bg-amber-400/10 text-amber-300", letter: "沙" },
  query: { ring: "border-rose-400/40", badge: "bg-rose-400/10 text-rose-300", letter: "查" },
};

export default function StageNode({ data, selected }: NodeProps<PipelineNodeData>) {
  const style = STAGE_STYLE[data.stage];
  return (
    <div
      className={`w-52 rounded-xl border bg-ink-800/95 px-4 py-3 shadow-xl transition ${style.ring} ${
        selected ? "ring-2 ring-accent/60" : ""
      }`}
    >
      <Handle type="target" position={Position.Left} className="!h-2 !w-2 !border-0 !bg-slate-600" />
      <div className="flex items-center gap-3">
        <span
          className={`flex h-9 w-9 items-center justify-center rounded-lg text-sm font-semibold ${style.badge}`}
        >
          {style.letter}
        </span>
        <div className="min-w-0">
          <p className="truncate text-sm font-semibold text-slate-100">{data.label}</p>
          <p className="truncate text-[11px] text-slate-500">{data.caption}</p>
        </div>
      </div>
      <div className="mt-3 flex items-center justify-between text-[10px] uppercase tracking-wider text-slate-500">
        <span>{data.type}</span>
        <span className="h-1.5 w-1.5 rounded-full bg-accent/70" />
      </div>
      <Handle type="source" position={Position.Right} className="!h-2 !w-2 !border-0 !bg-accent" />
    </div>
  );
}
