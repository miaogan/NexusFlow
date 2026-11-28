import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ReactFlowProvider,
  addEdge,
  useEdgesState,
  useNodesState,
  type Connection,
  type Edge,
  type OnEdgesChange,
  type OnNodesChange,
} from "reactflow";
import {
  createWorkflowDefinition,
  deleteWorkflowDefinition,
  duplicateWorkflowDefinition,
  listWorkflowDefinitionVersions,
  listWorkflowDefinitions,
  listWorkflowRuns,
  publishWorkflowDefinition,
  rollbackWorkflowDefinition,
  runDag,
  runSearch,
  unpublishWorkflowDefinition,
  updateWorkflowDefinition,
  validateDag,
} from "../api/client";
import Inspector from "../components/Inspector";
import Palette from "../components/Palette";
import WorkflowCanvas from "../components/WorkflowCanvas";
import { Button, PageHeader, Spinner, Tag } from "../components/ui";
import type {
  Dag,
  PaletteItem,
  PipelineNode,
  PipelineNodeData,
  SearchResponse,
  Stage,
  ValidationResult,
  WorkflowDefinition,
  WorkflowDefinitionVersion,
  WorkflowRunState,
} from "../types";

const PALETTE: PaletteItem[] = [
  { stage: "ingest", type: "ingest", label: "文件摄入", caption: "上传 / 预签名直传", description: "接收异构文件，写入对象存储并投递任务。" },
  { stage: "parse", type: "parse", label: "MinerU 解析", caption: "版面还原 Markdown", description: "异步解析 PDF / Word，产出结构化文本。" },
  { stage: "index", type: "index", label: "LlamaIndex 加工", caption: "切块 / 索引 / Embedding", description: "语义切块、向量与关键词索引构建。" },
  { stage: "sandbox", type: "sandbox", label: "代码沙箱", caption: "OpenSandbox 执行", description: "强隔离执行自定义代码节点。" },
  { stage: "query", type: "query", label: "检索查询", caption: "多路召回 + RRF + Rerank", description: "向量 / BM25 / 图谱三路召回。" },
];

const NODE_META: Record<string, { label: string; caption: string; stage: Stage }> = {
  ingest: { label: "文件摄入", caption: "MinIO 对象存储", stage: "ingest" },
  parse: { label: "MinerU 解析", caption: "输出结构化 Markdown", stage: "parse" },
  index: { label: "LlamaIndex 加工", caption: "切块 / 索引 / Embedding", stage: "index" },
  sandbox: { label: "代码沙箱", caption: "OpenSandbox 执行", stage: "sandbox" },
  query: { label: "检索查询", caption: "多路召回 + RRF + Rerank", stage: "query" },
};

const CATEGORY_LABELS: Record<string, string> = {
  all: "全部",
  ingest: "向量化",
  chat: "对话",
  pipeline: "流水线",
  custom: "自定义",
};

function pipelineNode(id: string, type: string, x: number, y: number): PipelineNode {
  const meta = NODE_META[type];
  return {
    id,
    type: "pipeline",
    position: { x, y },
    data: { label: meta.label, caption: meta.caption, stage: meta.stage, type },
  };
}

function chain(ids: string[]): Edge[] {
  return ids.slice(1).map((to, index) => ({
    id: `e-${ids[index]}-${to}`,
    source: ids[index],
    target: to,
    animated: true,
  }));
}

interface WorkflowTemplate {
  id: string;
  name: string;
  tag: string;
  summary: string;
  nodes: PipelineNode[];
  edges: Edge[];
}

const TEMPLATES: WorkflowTemplate[] = [
  {
    id: "vectorize",
    name: "文档向量化",
    tag: "ingest",
    summary: "上传 → 解析 → 切块索引，落地为可检索的知识库",
    nodes: [
      pipelineNode("ingest", "ingest", 40, 140),
      pipelineNode("parse", "parse", 300, 140),
      pipelineNode("index", "index", 560, 140),
    ],
    edges: chain(["ingest", "parse", "index"]),
  },
  {
    id: "chat",
    name: "对话问答",
    tag: "chat",
    summary: "多路召回 + RRF + Rerank，为对话提供引用与依据",
    nodes: [pipelineNode("query", "query", 40, 140)],
    edges: [],
  },
  {
    id: "parse",
    name: "文件解析",
    tag: "ingest",
    summary: "仅摄入与版面解析，输出结构化 Markdown",
    nodes: [pipelineNode("ingest", "ingest", 40, 140), pipelineNode("parse", "parse", 300, 140)],
    edges: chain(["ingest", "parse"]),
  },
  {
    id: "full",
    name: "全链路流水线",
    tag: "pipeline",
    summary: "摄入 / 解析 / 索引 + 沙箱 + 检索，多分支编排",
    nodes: [
      pipelineNode("ingest", "ingest", 40, 140),
      pipelineNode("parse", "parse", 300, 140),
      pipelineNode("index", "index", 560, 140),
      pipelineNode("sandbox", "sandbox", 300, 320),
      pipelineNode("query", "query", 560, 320),
    ],
    edges: [
      ...chain(["ingest", "parse", "index"]),
      { id: "e-index-query", source: "index", target: "query", animated: true },
      { id: "e-sandbox-query", source: "sandbox", target: "query", animated: true },
    ],
  },
  {
    id: "blank",
    name: "空白画布",
    tag: "custom",
    summary: "从零开始拖拽节点，自由编排 DAG",
    nodes: [],
    edges: [],
  },
];

const DEMO_TEMPLATE = TEMPLATES.find((template) => template.id === "full")!;

function buildDag(nodes: PipelineNode[], edges: Edge[], name: string): Dag {
  return {
    name: name.trim() || "nexusflow-pipeline",
    nodes: nodes.map((node) => ({ id: node.id, type: node.data.type, config: node.data.config })),
    edges: edges.map((edge) => ({ from: edge.source, to: edge.target })),
  };
}

function layoutOf(nodes: PipelineNode[]): Record<string, { x: number; y: number }> {
  return Object.fromEntries(nodes.map((node) => [node.id, { x: node.position.x, y: node.position.y }]));
}

/** Layered (longest-path) layout: rank columns, stack siblings vertically. */
function autoLayoutNodes(nodes: PipelineNode[], edges: Edge[]): PipelineNode[] {
  const ids = new Set(nodes.map((node) => node.id));
  const indegree = new Map<string, number>();
  const adjacency = new Map<string, string[]>();
  nodes.forEach((node) => {
    indegree.set(node.id, 0);
    adjacency.set(node.id, []);
  });
  edges.forEach((edge) => {
    if (!ids.has(edge.source) || !ids.has(edge.target)) return;
    adjacency.get(edge.source)?.push(edge.target);
    indegree.set(edge.target, (indegree.get(edge.target) ?? 0) + 1);
  });

  const rank = new Map<string, number>();
  const degree = new Map(indegree);
  const queue: string[] = [];
  nodes.forEach((node) => {
    if ((indegree.get(node.id) ?? 0) === 0) {
      queue.push(node.id);
      rank.set(node.id, 0);
    }
  });
  while (queue.length > 0) {
    const id = queue.shift() as string;
    for (const next of adjacency.get(id) ?? []) {
      rank.set(next, Math.max(rank.get(next) ?? 0, (rank.get(id) ?? 0) + 1));
      const remaining = (degree.get(next) ?? 1) - 1;
      degree.set(next, remaining);
      if (remaining === 0) queue.push(next);
    }
  }
  const maxRank = rank.size > 0 ? Math.max(...rank.values()) : 0;
  nodes.forEach((node) => {
    if (!rank.has(node.id)) rank.set(node.id, maxRank + 1);
  });

  const columns = new Map<number, string[]>();
  nodes.forEach((node) => {
    const level = rank.get(node.id) ?? 0;
    const list = columns.get(level) ?? [];
    list.push(node.id);
    columns.set(level, list);
  });

  const positions = new Map<string, { x: number; y: number }>();
  columns.forEach((list, level) => {
    list.sort();
    list.forEach((id, index) => {
      positions.set(id, { x: 60 + level * 250, y: 110 + index * 150 });
    });
  });
  return nodes.map((node) => ({ ...node, position: positions.get(node.id) ?? node.position }));
}

function errMsg(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function downloadJSON(data: unknown, filename: string) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  URL.revokeObjectURL(url);
}

function statusLabel(definition: WorkflowDefinition | null): string {
  if (!definition) return "未保存草稿";
  if (definition.status === "published") {
    return `已发布 v${definition.version}`;
  }
  return definition.version > 0 ? `草稿 · 基于 v${definition.version}` : "草稿";
}

export default function Workflow() {
  const [nodes, setNodes, onNodesChange] = useNodesState<PipelineNodeData>(DEMO_TEMPLATE.nodes);
  const [edges, setEdges, onEdgesChange] = useEdgesState(DEMO_TEMPLATE.edges);
  const nodeSeq = useRef(100);

  const [definitions, setDefinitions] = useState<WorkflowDefinition[]>([]);
  const [currentId, setCurrentId] = useState<string | null>(null);
  const [currentName, setCurrentName] = useState("");
  const [dirty, setDirty] = useState(true);
  const [loadingDefs, setLoadingDefs] = useState(true);
  const [query, setQuery] = useState("");
  const [category, setCategory] = useState("all");
  const [sortBy, setSortBy] = useState<"updated" | "name">("updated");

  const [showCreate, setShowCreate] = useState(false);
  const [createMode, setCreateMode] = useState<"template" | "draft">("template");
  const [templateId, setTemplateId] = useState("vectorize");
  const [draftName, setDraftName] = useState("");
  const [draftDesc, setDraftDesc] = useState("");

  const [renameTarget, setRenameTarget] = useState<WorkflowDefinition | null>(null);
  const [renameName, setRenameName] = useState("");
  const [versionsTarget, setVersionsTarget] = useState<WorkflowDefinition | null>(null);
  const [versions, setVersions] = useState<WorkflowDefinitionVersion[]>([]);
  const [versionsLoading, setVersionsLoading] = useState(false);

  const [runs, setRuns] = useState<WorkflowRunState[]>([]);
  const [runsLoading, setRunsLoading] = useState(false);

  const [selected, setSelected] = useState<PipelineNode | null>(null);
  const [search, setSearch] = useState<SearchResponse | null>(null);
  const [searching, setSearching] = useState(false);
  const [searchError, setSearchError] = useState<string | null>(null);
  const [validation, setValidation] = useState<ValidationResult | null>(null);
  const [validationError, setValidationError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [runOutput, setRunOutput] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [publishing, setPublishing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [fitSignal, setFitSignal] = useState(0);

  const importRef = useRef<HTMLInputElement>(null);

  const currentDefinition = useMemo(
    () => definitions.find((definition) => definition.id === currentId) ?? null,
    [definitions, currentId],
  );
  const published = currentDefinition?.status === "published";

  const flash = (message: string) => {
    setNotice(message);
    window.setTimeout(() => setNotice(null), 2400);
  };

  const refreshRuns = useCallback(async (definitionID: string | null) => {
    if (!definitionID) {
      setRuns([]);
      return;
    }
    setRunsLoading(true);
    try {
      const res = await listWorkflowRuns(20, definitionID);
      setRuns(res.runs ?? []);
    } catch {
      setRuns([]);
    } finally {
      setRunsLoading(false);
    }
  }, []);

  const loadDefinition = useCallback(
    (definition: WorkflowDefinition) => {
      const loadedNodes = definition.dag.nodes.map((node, index) => {
        const meta = NODE_META[node.type];
        return {
          id: node.id,
          type: "pipeline",
          position: definition.layout?.[node.id] ?? { x: 60 + (index % 3) * 250, y: 110 + Math.floor(index / 3) * 150 },
          data: {
            label: meta?.label ?? node.type,
            caption: meta?.caption ?? "",
            stage: meta?.stage ?? "index",
            type: node.type,
            config: node.config,
          },
        } satisfies PipelineNode;
      });
      const loadedEdges: Edge[] = definition.dag.edges.map((edge, index) => ({
        id: `e-${index}-${edge.from}-${edge.to}`,
        source: edge.from,
        target: edge.to,
        animated: true,
      }));
      setNodes(loadedNodes);
      setEdges(loadedEdges);
      setCurrentId(definition.id);
      setCurrentName(definition.name);
      setDirty(false);
      setSelected(null);
      setValidation(null);
      setValidationError(null);
      setRunOutput(null);
      setFitSignal((value) => value + 1);
      void refreshRuns(definition.id);
    },
    [setNodes, setEdges, refreshRuns],
  );

  const loadTemplate = useCallback(
    (template: WorkflowTemplate) => {
      setNodes(template.nodes);
      setEdges(template.edges);
      setSelected(null);
      setValidation(null);
      setValidationError(null);
      setRunOutput(null);
      setFitSignal((value) => value + 1);
    },
    [setNodes, setEdges],
  );

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await listWorkflowDefinitions();
        if (cancelled) return;
        setDefinitions(res.definitions);
        if (res.definitions.length > 0) {
          loadDefinition(res.definitions[0]);
        } else {
          setCurrentName("示例流水线（未保存）");
          setDirty(true);
        }
      } catch (error_) {
        if (!cancelled) setError(errMsg(error_));
      } finally {
        if (!cancelled) setLoadingDefs(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [loadDefinition]);

  const visibleDefinitions = useMemo(() => {
    const term = query.trim().toLowerCase();
    return definitions
      .filter((definition) => category === "all" || (definition.category || "custom") === category)
      .filter((definition) => !term || definition.name.toLowerCase().includes(term))
      .sort((a, b) =>
        sortBy === "name" ? a.name.localeCompare(b.name) : b.updated_at.localeCompare(a.updated_at),
      );
  }, [definitions, query, category, sortBy]);

  const replaceDefinition = (definition: WorkflowDefinition) => {
    setDefinitions((current) => {
      const exists = current.some((item) => item.id === definition.id);
      return exists
        ? current.map((item) => (item.id === definition.id ? definition : item))
        : [definition, ...current];
    });
  };

  const handleNodesChange: OnNodesChange = useCallback(
    (changes) => {
      onNodesChange(changes);
      if (changes.some((change) => change.type !== "select")) setDirty(true);
    },
    [onNodesChange],
  );

  const handleEdgesChange: OnEdgesChange = useCallback(
    (changes) => {
      onEdgesChange(changes);
      if (changes.some((change) => change.type !== "select")) setDirty(true);
    },
    [onEdgesChange],
  );

  const addNodeAt = useCallback(
    (item: PaletteItem, position: { x: number; y: number }) => {
      if (published) {
        flash("工作流已发布并锁定，请先转为草稿");
        return;
      }
      nodeSeq.current += 1;
      const id = `node-${nodeSeq.current}`;
      setNodes((current) => [
        ...current,
        { id, type: "pipeline", position, data: { label: item.label, caption: item.caption, stage: item.stage, type: item.type } },
      ]);
      setDirty(true);
    },
    [setNodes, published],
  );

  const handleAddFromPalette = (item: PaletteItem) => {
    const seq = nodeSeq.current + 1;
    addNodeAt(item, { x: 240 + (seq % 4) * 60, y: 380 + (seq % 3) * 70 });
  };

  const handleDropNode = (type: string, position: { x: number; y: number }) => {
    const item = PALETTE.find((entry) => entry.type === type);
    if (item) addNodeAt(item, position);
  };

  const onConnect = useCallback(
    (connection: Connection) => {
      if (published) return;
      setEdges((current) => addEdge(connection, current));
      setDirty(true);
    },
    [setEdges, published],
  );

  const handleSearch = async (searchQuery: string) => {
    setSearching(true);
    setSearchError(null);
    try {
      setSearch(await runSearch(searchQuery));
    } catch (error_) {
      setSearchError(errMsg(error_));
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
      setValidation(await validateDag(buildDag(nodes, edges, currentName)));
    } catch (error_) {
      setValidationError(errMsg(error_));
    } finally {
      setBusy(false);
    }
  };

  const handleRun = async () => {
    setBusy(true);
    setValidationError(null);
    setRunOutput(null);
    try {
      setRunOutput(
        JSON.stringify(await runDag(buildDag(nodes, edges, currentName), currentId ?? undefined), null, 2),
      );
      if (currentId) void refreshRuns(currentId);
    } catch (error_) {
      setValidationError(errMsg(error_));
    } finally {
      setBusy(false);
    }
  };

  const handleExport = () => {
    downloadJSON(buildDag(nodes, edges, currentName), `${currentName.trim() || "nexusflow-dag"}.json`);
  };

  const handleExportDefinition = (definition: WorkflowDefinition) => {
    downloadJSON(
      {
        name: definition.name,
        description: definition.description,
        category: definition.category,
        dag: definition.dag,
        layout: definition.layout,
      },
      `${definition.name}.json`,
    );
  };

  const handleBeautify = () => {
    if (published) {
      flash("工作流已发布并锁定，请先转为草稿");
      return;
    }
    setNodes((current) => autoLayoutNodes(current, edges));
    setFitSignal((value) => value + 1);
    setDirty(true);
    flash("已按依赖层级自动布局");
  };

  const openCreate = () => {
    setCreateMode("template");
    setTemplateId("vectorize");
    setDraftName("");
    setDraftDesc("");
    setShowCreate(true);
  };

  const openSaveDraft = () => {
    setCreateMode("draft");
    setDraftName(currentName.replace("（未保存）", "").trim());
    setDraftDesc("");
    setShowCreate(true);
  };

  const submitCreate = async () => {
    const name = draftName.trim();
    if (!name || saving) return;
    setSaving(true);
    setError(null);
    try {
      let created: WorkflowDefinition;
      if (createMode === "draft") {
        const res = await createWorkflowDefinition({
          name,
          description: draftDesc.trim() || undefined,
          category: "custom",
          dag: buildDag(nodes, edges, name),
          layout: layoutOf(nodes),
        });
        created = res.definition;
      } else {
        const template = TEMPLATES.find((entry) => entry.id === templateId) ?? TEMPLATES[0];
        const res = await createWorkflowDefinition({
          name,
          description: draftDesc.trim() || template.summary,
          category: template.tag,
          dag: buildDag(template.nodes, template.edges, name),
          layout: layoutOf(template.nodes),
        });
        created = res.definition;
      }
      replaceDefinition(created);
      setCurrentName(created.name);
      if (createMode === "template") loadDefinition(created);
      if (createMode !== "template") {
        setCurrentId(created.id);
        setDirty(false);
        void refreshRuns(created.id);
      }
      setShowCreate(false);
      flash(`已创建工作流「${created.name}」`);
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setSaving(false);
    }
  };

  const handleSave = async () => {
    if (!currentId) {
      openSaveDraft();
      return;
    }
    if (published) {
      setError("工作流已发布并锁定；先「转为草稿」再保存修改。");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const res = await updateWorkflowDefinition(currentId, {
        name: currentName,
        description: currentDefinition?.description,
        category: currentDefinition?.category,
        dag: buildDag(nodes, edges, currentName),
        layout: layoutOf(nodes),
      });
      replaceDefinition(res.definition);
      setCurrentName(res.definition.name);
      setDirty(false);
      flash("已保存当前草稿");
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setSaving(false);
    }
  };

  const handleSwitch = (definition: WorkflowDefinition) => {
    if (definition.id === currentId) return;
    if (dirty && !window.confirm("当前工作流有未保存修改，切换后丢失，继续？")) return;
    setError(null);
    loadDefinition(definition);
  };

  const handleDelete = async (definition: WorkflowDefinition) => {
    if (!window.confirm(`确定删除工作流「${definition.name}」吗？历史版本将一并删除。`)) return;
    setError(null);
    try {
      await deleteWorkflowDefinition(definition.id);
      const remaining = definitions.filter((item) => item.id !== definition.id);
      setDefinitions(remaining);
      if (currentId === definition.id) {
        if (remaining.length > 0) {
          loadDefinition(remaining[0]);
        } else {
          loadTemplate(DEMO_TEMPLATE);
          setCurrentId(null);
          setCurrentName("示例流水线（未保存）");
          setDirty(true);
          setRuns([]);
        }
      }
      flash(`已删除工作流「${definition.name}」`);
    } catch (error_) {
      setError(errMsg(error_));
    }
  };

  const handleDuplicate = async (definition: WorkflowDefinition) => {
    setError(null);
    try {
      const res = await duplicateWorkflowDefinition(definition.id);
      replaceDefinition(res.definition);
      loadDefinition(res.definition);
      flash(`已复制为「${res.definition.name}」`);
    } catch (error_) {
      setError(errMsg(error_));
    }
  };

  const submitRename = async () => {
    const target = renameTarget;
    const name = renameName.trim();
    if (!target || !name || publishing) return;
    setPublishing(true);
    setError(null);
    try {
      const res = await updateWorkflowDefinition(target.id, {
        name,
        description: target.description,
        category: target.category,
        dag: target.dag,
        layout: target.layout,
      });
      replaceDefinition(res.definition);
      if (target.id === currentId) setCurrentName(res.definition.name);
      setRenameTarget(null);
      flash("已重命名");
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setPublishing(false);
    }
  };

  const togglePublish = async () => {
    if (!currentDefinition || publishing) return;
    setPublishing(true);
    setError(null);
    try {
      if (published) {
        if (!window.confirm("转为草稿后可继续编辑；已发布版本会保留在历史中。继续？")) {
          setPublishing(false);
          return;
        }
        const res = await unpublishWorkflowDefinition(currentDefinition.id);
        replaceDefinition(res.definition);
        flash("已转为草稿，可继续编辑");
      } else {
        const res = await publishWorkflowDefinition(currentDefinition.id);
        replaceDefinition(res.definition);
        setDirty(false);
        flash(`已发布 v${res.version}`);
        void refreshRuns(res.definition.id);
      }
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setPublishing(false);
    }
  };

  const openVersions = async (definition: WorkflowDefinition) => {
    setVersionsTarget(definition);
    setVersions([]);
    setVersionsLoading(true);
    setError(null);
    try {
      const res = await listWorkflowDefinitionVersions(definition.id);
      setVersions(res.versions);
    } catch (error_) {
      setError(errMsg(error_));
    } finally {
      setVersionsLoading(false);
    }
  };

  const handleRollback = async (version: number) => {
    if (!versionsTarget) return;
    if (!window.confirm(`回滚到 v${version}？当前内容会替换为该版本的草稿。`)) return;
    setError(null);
    try {
      const res = await rollbackWorkflowDefinition(versionsTarget.id, version);
      replaceDefinition(res.definition);
      loadDefinition(res.definition);
      setVersionsTarget(null);
      flash(`已回滚到 v${version}，生成草稿`);
    } catch (error_) {
      setError(errMsg(error_));
    }
  };

  const handleImport = async (file: File | null) => {
    if (!file) return;
    setError(null);
    try {
      const parsed = JSON.parse(await file.text()) as Record<string, unknown>;
      const rawDag = (parsed.dag ?? parsed) as Partial<Dag> | null;
      if (!rawDag || !Array.isArray(rawDag.nodes)) {
        throw new Error("文件缺少 dag.nodes");
      }
      const name =
        (typeof parsed.name === "string" && parsed.name.trim()) ||
        file.name.replace(/\.json$/i, "") ||
        "导入的工作流";
      const res = await createWorkflowDefinition({
        name,
        description: typeof parsed.description === "string" ? parsed.description : undefined,
        category: typeof parsed.category === "string" ? parsed.category : "custom",
        dag: {
          name,
          nodes: rawDag.nodes,
          edges: Array.isArray(rawDag.edges) ? rawDag.edges : [],
        },
        layout:
          parsed.layout && typeof parsed.layout === "object"
            ? (parsed.layout as Record<string, { x: number; y: number }>)
            : undefined,
      });
      replaceDefinition(res.definition);
      loadDefinition(res.definition);
      flash(`已导入「${res.definition.name}」`);
    } catch (error_) {
      setError(`导入失败：${errMsg(error_)}`);
    } finally {
      if (importRef.current) importRef.current.value = "";
    }
  };

  const currentTemplate = TEMPLATES.find((entry) => entry.id === templateId) ?? TEMPLATES[0];

  return (
    <div>
      <PageHeader
        title="工作流编排"
        description={`${currentName || "未命名工作流"} · 拖拽节点、连线编排 DAG，校验后试运行`}
        actions={
          <>
            {dirty && <span className="text-[10px] text-amber-300">未保存</span>}
            <Button variant="ghost" onClick={handleExport}>导出 DAG</Button>
            <Button variant="ghost" onClick={handleBeautify} disabled={published}>一键美化</Button>
            <Button variant="subtle" onClick={() => void handleSave()} disabled={saving || busy}>
              {saving ? "保存中…" : "保存"}
            </Button>
            <Button onClick={openCreate}>新建工作流</Button>
          </>
        }
      />

      {error && <div className="panel mb-3 p-3 text-xs text-rose-300">{error}</div>}
      {notice && <div className="panel mb-3 p-3 text-xs text-emerald-300">{notice}</div>}

      <div className="grid h-[calc(100vh-190px)] grid-cols-1 gap-4 lg:grid-cols-[240px_minmax(0,1fr)_330px]">
        <div className="hidden min-h-0 flex-col gap-4 lg:flex">
          <section className="panel flex max-h-[52%] min-h-0 flex-col overflow-hidden">
            <div className="border-b border-white/5 px-3 py-2.5">
              <div className="flex items-center justify-between">
                <p className="panel-title">工作流库</p>
                <div className="flex items-center gap-1">
                  <input
                    ref={importRef}
                    type="file"
                    accept="application/json,.json"
                    className="hidden"
                    onChange={(event) => void handleImport(event.target.files?.[0] ?? null)}
                  />
                  <Button
                    variant="ghost"
                    className="px-2 py-1 text-[10px]"
                    onClick={() => importRef.current?.click()}
                  >
                    导入
                  </Button>
                  <Button className="px-2 py-1 text-[10px]" onClick={openCreate}>新建</Button>
                </div>
              </div>
              <div className="mt-2 space-y-1.5">
                <input
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  placeholder="搜索工作流名称"
                  className="w-full rounded-lg border border-white/10 bg-ink-950 px-2 py-1 text-[11px] text-slate-200 outline-none focus:border-accent/40"
                />
                <div className="flex flex-wrap gap-1">
                  {Object.entries(CATEGORY_LABELS).map(([value, label]) => (
                    <button
                      key={value}
                      type="button"
                      onClick={() => setCategory(value)}
                      className={`rounded px-1.5 py-0.5 text-[9px] transition ${
                        category === value
                          ? "bg-accent/15 text-accent"
                          : "text-slate-500 hover:bg-white/5 hover:text-slate-300"
                      }`}
                    >
                      {label}
                    </button>
                  ))}
                </div>
                <div className="flex items-center justify-between text-[9px] text-slate-600">
                  <span>{visibleDefinitions.length} / {definitions.length} 个工作流</span>
                  <select
                    value={sortBy}
                    onChange={(event) => setSortBy(event.target.value as "updated" | "name")}
                    className="rounded border border-white/10 bg-ink-950 px-1 py-0.5 text-[9px] text-slate-300 outline-none"
                  >
                    <option value="updated">最近更新</option>
                    <option value="name">按名称</option>
                  </select>
                </div>
              </div>
            </div>
            <div className="min-h-0 flex-1 space-y-1.5 overflow-y-auto p-2">
              {loadingDefs ? (
                <div className="flex items-center gap-2 px-2 py-3 text-[11px] text-slate-500">
                  <Spinner className="h-3 w-3" /> 加载中…
                </div>
              ) : visibleDefinitions.length === 0 ? (
                <p className="px-2 py-3 text-[11px] leading-relaxed text-slate-500">
                  {definitions.length === 0
                    ? "还没有工作流。点击「新建」从模板创建，或「导入」JSON。"
                    : "没有匹配的工作流，调整搜索或分类。"}
                </p>
              ) : (
                visibleDefinitions.map((definition) => {
                  const active = currentId === definition.id;
                  return (
                    <div
                      key={definition.id}
                      className={`group cursor-pointer rounded-lg border p-2 transition ${
                        active
                          ? "border-accent/40 bg-accent/5"
                          : "border-white/5 bg-white/[0.02] hover:border-white/10"
                      }`}
                      onClick={() => handleSwitch(definition)}
                    >
                      <div className="flex items-center gap-1.5">
                        <span className="min-w-0 flex-1 truncate text-[11px] font-medium text-slate-200">
                          {definition.name}
                        </span>
                        <span
                          className={`shrink-0 rounded px-1 py-0.5 text-[9px] ${
                            definition.status === "published"
                              ? "bg-emerald-500/10 text-emerald-300"
                              : "bg-white/5 text-slate-400"
                          }`}
                        >
                          {definition.status === "published" ? `v${definition.version}` : "草稿"}
                        </span>
                      </div>
                      <div className="mt-1 flex items-center gap-1.5">
                        <Tag>{definition.category || "custom"}</Tag>
                        <span className="text-[9px] text-slate-600">
                          {definition.dag.nodes.length} 节点
                        </span>
                        <span className="ml-auto text-[9px] text-slate-600">
                          {definition.updated_at?.replace("T", " ").slice(5, 16)}
                        </span>
                      </div>
                      {active && (
                        <div
                          className="mt-1.5 flex flex-wrap gap-1"
                          onClick={(event) => event.stopPropagation()}
                        >
                          <button
                            type="button"
                            onClick={() => {
                              setRenameTarget(definition);
                              setRenameName(definition.name);
                            }}
                            className="rounded px-1.5 py-0.5 text-[9px] text-slate-400 hover:bg-white/5 hover:text-slate-200"
                          >
                            重命名
                          </button>
                          <button
                            type="button"
                            onClick={() => void handleDuplicate(definition)}
                            className="rounded px-1.5 py-0.5 text-[9px] text-slate-400 hover:bg-white/5 hover:text-slate-200"
                          >
                            复制
                          </button>
                          <button
                            type="button"
                            onClick={() => handleExportDefinition(definition)}
                            className="rounded px-1.5 py-0.5 text-[9px] text-slate-400 hover:bg-white/5 hover:text-slate-200"
                          >
                            导出
                          </button>
                          <button
                            type="button"
                            onClick={() => void openVersions(definition)}
                            className="rounded px-1.5 py-0.5 text-[9px] text-slate-400 hover:bg-white/5 hover:text-slate-200"
                          >
                            历史
                          </button>
                          <button
                            type="button"
                            onClick={() => void handleDelete(definition)}
                            className="rounded px-1.5 py-0.5 text-[9px] text-rose-300 hover:bg-rose-500/10"
                          >
                            删除
                          </button>
                        </div>
                      )}
                    </div>
                  );
                })
              )}
            </div>
          </section>
          <div className="min-h-0 flex-1">
            <Palette items={PALETTE} onAdd={handleAddFromPalette} />
          </div>
        </div>

        <section className="panel relative overflow-hidden">
          <div className="pointer-events-none absolute left-4 top-4 z-10">
            <p className="text-[10px] uppercase tracking-[0.2em] text-slate-500">Workflow Canvas</p>
            <p className="mt-1 text-[10px] text-slate-600">拖拽节点 · 连线编排 · 校验执行</p>
          </div>
          <div className="absolute right-4 top-4 z-10 flex items-center gap-2 rounded-lg border border-white/10 bg-ink-950/85 px-2 py-1.5 backdrop-blur">
            <span
              className={`text-[10px] ${
                published ? "text-emerald-300" : "text-slate-400"
              }`}
            >
              {statusLabel(currentDefinition)}
            </span>
            {currentId && (
              <>
                <Button
                  variant="ghost"
                  className="px-2 py-1 text-[10px]"
                  onClick={() => void togglePublish()}
                  disabled={publishing}
                >
                  {published ? "转为草稿" : "发布"}
                </Button>
                <Button
                  variant="ghost"
                  className="px-2 py-1 text-[10px]"
                  onClick={() => currentDefinition && void openVersions(currentDefinition)}
                >
                  历史版本
                </Button>
              </>
            )}
          </div>
          <ReactFlowProvider>
            <WorkflowCanvas
              nodes={nodes}
              edges={edges}
              onNodesChange={handleNodesChange}
              onEdgesChange={handleEdgesChange}
              onConnect={onConnect}
              onSelect={setSelected}
              onDropNode={handleDropNode}
              fitSignal={fitSignal}
              readOnly={published}
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
            runs={runs}
            runsLoading={runsLoading}
          />
        </div>
      </div>

      {showCreate && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
          onClick={() => !saving && setShowCreate(false)}
        >
          <div
            className="panel flex max-h-[86vh] w-full max-w-2xl flex-col p-5"
            onClick={(event) => event.stopPropagation()}
          >
            <div className="mb-4 flex items-center justify-between">
              <div>
                <h3 className="panel-title">
                  {createMode === "template" ? "新建工作流" : "保存为新的工作流"}
                </h3>
                <p className="mt-1 text-[10px] text-slate-500">
                  {createMode === "template"
                    ? "选择一个模板作为起点，创建后仍可自由增删节点。"
                    : "把当前画布保存为一条可复用的工作流草稿。"}
                </p>
              </div>
              <button
                type="button"
                onClick={() => setShowCreate(false)}
                className="rounded px-2 py-0.5 text-xs text-slate-400 hover:bg-white/5 hover:text-slate-200"
              >
                关闭
              </button>
            </div>

            {createMode === "template" && (
              <div className="mb-4 grid gap-2 overflow-y-auto sm:grid-cols-2">
                {TEMPLATES.map((template) => (
                  <button
                    key={template.id}
                    type="button"
                    onClick={() => setTemplateId(template.id)}
                    className={`rounded-lg border p-3 text-left transition ${
                      templateId === template.id
                        ? "border-accent/40 bg-accent/5"
                        : "border-white/10 bg-white/[0.02] hover:border-white/20"
                    }`}
                  >
                    <div className="flex items-center gap-2">
                      <span className="text-xs font-medium text-slate-200">{template.name}</span>
                      <Tag>{template.tag}</Tag>
                    </div>
                    <p className="mt-1 text-[10px] leading-relaxed text-slate-500">{template.summary}</p>
                    <p className="mt-1 text-[9px] text-slate-600">{template.nodes.length} 个节点</p>
                  </button>
                ))}
              </div>
            )}

            <div className="space-y-2">
              <input
                value={draftName}
                onChange={(event) => setDraftName(event.target.value)}
                placeholder="工作流名称，例如：产品文档向量化"
                className="w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40"
              />
              <textarea
                value={draftDesc}
                onChange={(event) => setDraftDesc(event.target.value)}
                rows={2}
                placeholder="描述（可选）"
                className="w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40"
              />
            </div>

            <div className="mt-4 flex items-center justify-end gap-2">
              <Button variant="ghost" onClick={() => setShowCreate(false)} disabled={saving}>
                取消
              </Button>
              <Button onClick={() => void submitCreate()} disabled={saving || !draftName.trim()}>
                {saving ? "创建中…" : createMode === "template" ? `使用「${currentTemplate.name}」创建` : "保存"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {renameTarget && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
          onClick={() => !publishing && setRenameTarget(null)}
        >
          <div className="panel w-full max-w-md p-5" onClick={(event) => event.stopPropagation()}>
            <h3 className="panel-title">重命名工作流</h3>
            <p className="mt-1 text-[10px] text-slate-500">
              已发布的工作流需先转为草稿才能重命名。
            </p>
            <input
              value={renameName}
              onChange={(event) => setRenameName(event.target.value)}
              className="mt-3 w-full rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40"
            />
            <div className="mt-4 flex justify-end gap-2">
              <Button variant="ghost" onClick={() => setRenameTarget(null)} disabled={publishing}>
                取消
              </Button>
              <Button onClick={() => void submitRename()} disabled={publishing || !renameName.trim()}>
                {publishing ? "保存中…" : "保存"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {versionsTarget && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
          onClick={() => setVersionsTarget(null)}
        >
          <div
            className="panel flex max-h-[80vh] w-full max-w-lg flex-col p-5"
            onClick={(event) => event.stopPropagation()}
          >
            <div className="mb-3 flex items-center justify-between">
              <div>
                <h3 className="panel-title">历史版本 · {versionsTarget.name}</h3>
                <p className="mt-1 text-[10px] text-slate-500">
                  回滚会以该版本内容生成草稿，已发布版本不会被删除。
                </p>
              </div>
              <button
                type="button"
                onClick={() => setVersionsTarget(null)}
                className="rounded px-2 py-0.5 text-xs text-slate-400 hover:bg-white/5 hover:text-slate-200"
              >
                关闭
              </button>
            </div>
            <div className="min-h-0 flex-1 space-y-1.5 overflow-y-auto">
              {versionsLoading ? (
                <div className="flex items-center gap-2 py-4 text-xs text-slate-500">
                  <Spinner className="h-3 w-3" /> 加载中…
                </div>
              ) : versions.length === 0 ? (
                <p className="py-4 text-xs text-slate-500">还没有发布记录，发布后可在此回滚。</p>
              ) : (
                versions.map((version) => (
                  <div
                    key={version.version}
                    className="flex items-center gap-2 rounded-lg border border-white/5 bg-white/[0.02] p-2.5"
                  >
                    <span className="rounded bg-accent/10 px-1.5 py-0.5 text-[10px] text-accent">
                      v{version.version}
                    </span>
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-[11px] text-slate-200">{version.name}</p>
                      <p className="text-[9px] text-slate-500">
                        {version.dag.nodes.length} 节点 ·{" "}
                        {version.published_at?.replace("T", " ").slice(0, 16)}
                      </p>
                    </div>
                    <Button
                      variant="ghost"
                      className="px-2 py-1 text-[10px]"
                      onClick={() => void handleRollback(version.version)}
                    >
                      回滚
                    </Button>
                  </div>
                ))
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
