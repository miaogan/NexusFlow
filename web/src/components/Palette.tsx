import type { PaletteItem, Stage } from "../types";

const DOT: Record<Stage, string> = {
  ingest: "bg-sky-400",
  parse: "bg-violet-400",
  index: "bg-accent",
  sandbox: "bg-amber-400",
  query: "bg-rose-400",
  control: "bg-cyan-400",
  agent: "bg-fuchsia-400",
  human: "bg-orange-400",
  data: "bg-slate-400",
};

interface Props {
  items: PaletteItem[];
  onAdd: (item: PaletteItem) => void;
}

export default function Palette({ items, onAdd }: Props) {
  return (
    <aside className="panel flex h-full flex-col overflow-hidden">
      <div className="border-b border-white/5 px-4 py-3">
        <p className="panel-title">节点库</p>
        <p className="mt-1 text-[11px] text-slate-500">拖拽或点击添加到画布</p>
      </div>
      <div className="flex-1 space-y-2 overflow-y-auto p-3">
        {items.map((item) => (
          <button
            key={item.type}
            type="button"
            draggable
            onDragStart={(event) => {
              event.dataTransfer.setData("application/nexusflow-node", item.type);
              event.dataTransfer.effectAllowed = "move";
            }}
            onClick={() => onAdd(item)}
            className="group w-full cursor-grab rounded-xl border border-white/5 bg-ink-800/60 px-3 py-2.5 text-left transition hover:border-accent/30 hover:bg-ink-800 active:cursor-grabbing"
          >
            <div className="flex items-center gap-2">
              <span className={`h-1.5 w-1.5 rounded-full ${DOT[item.stage]}`} />
              <span className="text-xs font-semibold text-slate-200">{item.label}</span>
            </div>
            <p className="mt-1 text-[10px] leading-relaxed text-slate-500">{item.description}</p>
          </button>
        ))}
      </div>
      <div className="border-t border-white/5 px-4 py-3 text-[10px] leading-relaxed text-slate-500">
        DAG 由 Go Workflow Engine 解析，Python 服务负责切块、索引与检索。
      </div>
    </aside>
  );
}
