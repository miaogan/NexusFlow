import { useCallback, useRef, useState } from "react";
import {
  ReactFlowProvider,
  addEdge,
  useEdgesState,
  useNodesState,
  type Connection,
  type Edge,
} from "reactflow";
import { runDag, runSearch, validateDag } from "../api/client";
import Inspector from "../components/Inspector";
import Palette from "../components/Palette";
import WorkflowCanvas from "../components/WorkflowCanvas";
import { Button, PageHeader } from "../components/ui";
import type {
  Dag,
  PaletteItem,
  PipelineNode,
  PipelineNodeData,
  SearchResponse,
  ValidationResult,
} from "../types";

const PALETTE: PaletteItem[] = [
  { stage: "ingest", type: "ingest", label: "文件摄入", caption: "上传 / 预签名直传", description: "接收异构文件，写入对象存储并投递任务。" },
  { stage: "parse", type: "parse", label: "MinerU 解析", caption: "版面还原 Markdown", description: "异步解析 PDF / Word，产出结构化文本。" },
  { stage: "index", type: "index", label: "LlamaIndex 加工", caption: "切块 / 索引 / Embedding", description: "语义切块、向量与关键词索引构建。" },
  { stage: "sandbox", type: "sandbox", label: "代码沙箱", caption: "OpenSandbox 执行", description: "强隔离执行自定义代码节点。" },
  { stage: "query", type: "query", label: "检索查询", caption: "多路召回 + RRF + Rerank", description: "向量 / BM25 / 图谱三路召回。" },
];

const initialNodes: PipelineNode[] = [
  { id: "ingest", type: "pipeline", position: { x: 40, y: 120 }, data: { label: "文件摄入", caption: "MinIO 对象存储", stage: "ingest", type: "ingest" } },
  { id: "parse", type: "pipeline", position: { x: 300, y: 120 }, data: { label: "MinerU 解析", caption: "输出结构化 Markdown", stage: "parse", type: "parse" } },
  { id: "index", type: "pipeline", position: { x: 560, y: 120 }, data: { label: "LlamaIndex 加工", caption: "切块 / 索引 / Embedding", stage: "index", type: "index" } },
  { id: "sandbox", type: "pipeline", position: { x: 300, y: 300 }, data: { label: "代码沙箱", caption: "OpenSandbox 执行", stage: "sandbox", type: "sandbox" } },
  { id: "query", type: "pipeline", position: { x: 560, y: 300 }, data: { label: "检索查询", caption: "多路召回 + RRF + Rerank", stage: "query", type: "query" } },
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
    nodes: nodes.map((n) => ({ id: n.id, type: n.data.type, config: n.data.config })),
    edges: edges.map((e) => ({ from: e.source, to: e.target })),
  };
}

export default function Workflow() {
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

  const addNodeAt = useCallback((item: PaletteItem, position: { x: number; y: number }) => {
    nodeSeq.current += 1;
    const id = `node-${nodeSeq.current}`;
    setNodes((current) => [
      ...current,
      { id, type: "pipeline", position, data: { label: item.label, caption: item.caption, stage: item.stage, type: item.type } },
    ]);
  }, [setNodes]);

  const handleAddFromPalette = (item: PaletteItem) => {
    const seq = nodeSeq.current + 1;
    addNodeAt(item, { x: 240 + (seq % 4) * 60, y: 380 + (seq % 3) * 70 });
  };

  const handleDropNode = (type: string, position: { x: number; y: number }) => {
    const item = PALETTE.find((c) => c.type === type);
    if (item) addNodeAt(item, position);
  };

  const onConnect = useCallback((connection: Connection) => setEdges((current) => addEdge(connection, current)), [setEdges]);

  const handleSearch = async (query: string) => {
    setSearching(true); setSearchError(null);
    try { setSearch(await runSearch(query)); }
    catch (e) { setSearchError(e instanceof Error ? e.message : String(e)); }
    finally { setSearching(false); }
  };

  const handleValidate = async () => {
    setBusy(true); setValidation(null); setValidationError(null); setRunOutput(null);
    try { setValidation(await validateDag(buildDag(nodes, edges))); }
    catch (e) { setValidationError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  };

  const handleRun = async () => {
    setBusy(true); setValidationError(null); setRunOutput(null);
    try { setRunOutput(JSON.stringify(await runDag(buildDag(nodes, edges)), null, 2)); }
    catch (e) { setValidationError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  };

  const handleExport = () => {
    const blob = new Blob([JSON.stringify(buildDag(nodes, edges), null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url; a.download = "nexusflow-dag.json"; a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div>
      <PageHeader
        title="工作流编排"
        description="拖拽节点、连线编排 DAG，校验拓扑后试运行"
        actions={<Button variant="ghost" onClick={handleExport}>导出 DAG</Button>}
      />
      <div className="grid h-[calc(100vh-170px)] grid-cols-1 gap-4 lg:grid-cols-[220px_minmax(0,1fr)_330px]">
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
              nodes={nodes} edges={edges}
              onNodesChange={onNodesChange} onEdgesChange={onEdgesChange}
              onConnect={onConnect} onSelect={setSelected} onDropNode={handleDropNode}
            />
          </ReactFlowProvider>
        </section>
        <div className="hidden lg:block">
          <Inspector
            selected={selected} search={search} searching={searching} searchError={searchError}
            onSearch={handleSearch} validation={validation} validationError={validationError}
            onValidate={handleValidate} onRun={handleRun} busy={busy} runOutput={runOutput}
          />
        </div>
      </div>
    </div>
  );
}
