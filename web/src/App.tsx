import { useCallback, useRef, useState } from "react";
import {
  ReactFlowProvider,
  addEdge,
  useEdgesState,
  useNodesState,
  type Connection,
  type Edge,
} from "reactflow";
import { runDag, runSearch, validateDag } from "./api/client";
import Inspector from "./components/Inspector";
import Palette from "./components/Palette";
import WorkflowCanvas from "./components/WorkflowCanvas";
import type {
  Dag,
  PaletteItem,
  PipelineNode,
  PipelineNodeData,
  SearchResponse,
  ValidationResult,
} from "./types";

const PALETTE: PaletteItem[] = [
  {
    stage: "ingest",
    type: "ingest",
    label: "文件摄入",
    caption: "上传 / 预签名直传",
    description: "接收异构文件，写入 MinIO 并投递任务。",
  },
  {
    stage: "parse",
    type: "parse",
    label: "MinerU 解析",
    caption: "版面还原 Markdown",
    description: "异步解析 PDF / Word，产出结构化文本。",
  },
  {
    stage: "index",
    type: "index",
    label: "LlamaIndex 加工",
    caption: "切块 / 索引 / Embedding",
    description: "语义切块、向量与关键词索引构建。",
  },
  {
    stage: "sandbox",
    type: "sandbox",
    label: "代码沙箱",
    caption: "OpenSandbox 执行",
    description: "强隔离执行自定义代码节点。",
  },
  {
    stage: "query",
    type: "query",
    label: "检索查询",
    caption: "多路召回 + RRF + Rerank",
    description: "向量 / BM25 / 图谱三路召回。",
  },
];

const initialNodes: PipelineNode[] = [
  {
    id: "ingest",
    type: "pipeline",
    position: { x: 40, y: 150 },
    data: { label: "文件摄入", caption: "MinIO 对象存储", stage: "ingest", type: "ingest" },
  },
  {
    id: "parse",
    type: "pipeline",
    position: { x: 300, y: 150 },
    data: { label: "MinerU 解析", caption: "输出结构化 Markdown", stage: "parse", type: "parse" },
  },
  {
    id: "index",
    type: "pipeline",
    position: { x: 560, y: 150 },
    data: { label: "LlamaIndex 加工", caption: "切块 / 索引 / Embedding", stage: "index", type: "index" },
  },
  {
    id: "sandbox",
    type: "pipeline",
    position: { x: 300, y: 340 },
    data: { label: "代码沙箱", caption: "OpenSandbox 执行", stage: "sandbox", type: "sandbox" },
  },
  {
    id: "query",
    type: "pipeline",
    position: { x: 560, y: 340 },
    data: { label: "检索查询", caption: "多路召回 + RRF + Rerank", stage: "query", type: "query" },
  },
];

const initialEdges: Edge[] = [
  { id: "e-ingest-parse", source: "ingest", target: "parse", animated: true },
  { id: "e-parse-index", source: "parse", target: "index", animated: true },
  { id: "e-index-query", source: "index", target: "query", animated: true },
  { id: "e-sandbox-query", source: "sandbox", target: "query", animated: true },
];

function buildDag(nodes: PipelineNode[], edges: Edge[]): Dag {
  return {
    name: "nexusflow-pipeline",
    nodes: nodes.map((node) => ({ id: node.id, type: node.data.type, config: node.data.config })),
    edges: edges.map((edge) => ({ from: edge.source, to: edge.target })),
  };
}

export default function App() {
  const [nodes, setNodes, onNodesChange] = useNodesState<PipelineNodeData>(initialNodes);
  const [edges, setEdges, onEdgesChange] = useEdgesState(initialEdges);
  const nodeSeq = useRef(100);

  const [selected, setSelected] = useState<PipelineNode | null>(null);
  const [search, setSearch] = useState<SearchResponse | null>(null);
  const [searching, setSearching] = useState(false);
  const [searchError, setSearchError] = useState<string | null>(null);
  const [validation, setValidation] = useState<ValidationResult | null>(null);
  const [validationError, setValidationError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [runOutput, setRunOutput] = useState<string | null>(null);

  const addNodeAt = useCallback(
    (item: PaletteItem, position: { x: number; y: number }) => {
      nodeSeq.current += 1;
      const id = `node-${nodeSeq.current}`;
      setNodes((current) => [
        ...current,
        {
          id,
          type: "pipeline",
          position,
          data: {
            label: item.label,
            caption: item.caption,
            stage: item.stage,
            type: item.type,
          },
        },
      ]);
    },
    [setNodes],
  );

  const handleAddFromPalette = (item: PaletteItem) => {
    const seq = nodeSeq.current + 1;
    addNodeAt(item, { x: 240 + (seq % 4) * 60, y: 420 + (seq % 3) * 70 });
  };

  const handleDropNode = (type: string, position: { x: number; y: number }) => {
    const item = PALETTE.find((candidate) => candidate.type === type);
    if (item) addNodeAt(item, position);
  };

  const onConnect = useCallback(
    (connection: Connection) => setEdges((current) => addEdge(connection, current)),
    [setEdges],
  );

  const handleSearch = async (query: string) => {
    setSearching(true);
    setSearchError(null);
    try {
      setSearch(await runSearch(query));
    } catch (error) {
      setSearchError(error instanceof Error ? error.message : String(error));
    } finally {
      setSearching(false);
    }
  };

  const handleValidate = async () => {
    setBusy(true);
    setValidation(null);
    setValidationError(null);
    setRunOutput(null);
    try {
      setValidation(await validateDag(buildDag(nodes, edges)));
    } catch (error) {
      setValidationError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  const handleRun = async () => {
    setBusy(true);
    setValidationError(null);
    setRunOutput(null);
    try {
      const result = await runDag(buildDag(nodes, edges));
      setRunOutput(JSON.stringify(result, null, 2));
    } catch (error) {
      setValidationError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  const handleExport = () => {
    const blob = new Blob([JSON.stringify(buildDag(nodes, edges), null, 2)], {
      type: "application/json",
    });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "nexusflow-dag.json";
    anchor.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div className="flex h-screen flex-col">
      <header className="flex items-center justify-between border-b border-white/5 px-6 py-3">
        <div className="flex items-center gap-3">
          <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-accent/15 text-sm font-bold text-accent">
            N
          </div>
          <div>
            <h1 className="text-sm font-semibold tracking-wide text-slate-100">NexusFlow</h1>
            <p className="text-[10px] text-slate-500">分布式文件向量化工作流 · V4.0</p>
          </div>
        </div>
        <div className="flex items-center gap-3">
          <span className="hidden items-center gap-2 text-[10px] text-slate-500 sm:flex">
            <span className="h-1.5 w-1.5 rounded-full bg-emerald-400" />
            Go · Python · OpenSandbox
          </span>
          <button
            type="button"
            onClick={handleExport}
            className="rounded-lg border border-white/10 px-3 py-1.5 text-[11px] text-slate-300 transition hover:border-accent/40 hover:text-accent"
          >
            导出 DAG
          </button>
        </div>
      </header>

      <main className="grid flex-1 grid-cols-1 gap-4 overflow-hidden p-4 lg:grid-cols-[220px_minmax(0,1fr)_330px]">
        <div className="hidden lg:block">
          <Palette items={PALETTE} onAdd={handleAddFromPalette} />
        </div>

        <section className="panel relative overflow-hidden">
          <div className="pointer-events-none absolute left-4 top-4 z-10">
            <p className="text-[10px] uppercase tracking-[0.2em] text-slate-500">Workflow Canvas</p>
            <p className="mt-1 text-[10px] text-slate-600">拖拽节点 · 连线编排 · 校验执行</p>
          </div>
          <ReactFlowProvider>
            <WorkflowCanvas
              nodes={nodes}
              edges={edges}
              onNodesChange={onNodesChange}
              onEdgesChange={onEdgesChange}
              onConnect={onConnect}
              onSelect={setSelected}
              onDropNode={handleDropNode}
            />
          </ReactFlowProvider>
        </section>

        <div className="hidden lg:block">
          <Inspector
            selected={selected}
            search={search}
            searching={searching}
            searchError={searchError}
            onSearch={handleSearch}
            validation={validation}
            validationError={validationError}
            onValidate={handleValidate}
            onRun={handleRun}
            busy={busy}
            runOutput={runOutput}
          />
        </div>
      </main>

      <footer className="flex items-center justify-between border-t border-white/5 px-6 py-2 text-[10px] text-slate-500">
        <span>
          {nodes.length} nodes · {edges.length} edges
        </span>
        <span>RRF k=60 · Cross-Encoder Rerank · 三路召回</span>
      </footer>
    </div>
  );
}
