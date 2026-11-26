import type { DragEvent } from "react";
import ReactFlow, {
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  type NodeTypes,
  type OnConnect,
  type OnEdgesChange,
  type OnNodesChange,
  useReactFlow,
} from "reactflow";
import type { Edge, PipelineNode } from "../types";
import StageNode from "./StageNode";

const nodeTypes = { pipeline: StageNode } as NodeTypes;

interface Props {
  nodes: PipelineNode[];
  edges: Edge[];
  onNodesChange: OnNodesChange;
  onEdgesChange: OnEdgesChange;
  onConnect: OnConnect;
  onSelect: (node: PipelineNode | null) => void;
  onDropNode: (type: string, position: { x: number; y: number }) => void;
}

export default function WorkflowCanvas({
  nodes,
  edges,
  onNodesChange,
  onEdgesChange,
  onConnect,
  onSelect,
  onDropNode,
}: Props) {
  const { screenToFlowPosition } = useReactFlow();

  const handleDrop = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    const type = event.dataTransfer.getData("application/nexusflow-node");
    if (!type) return;
    const position = screenToFlowPosition({ x: event.clientX, y: event.clientY });
    onDropNode(type, { x: position.x - 100, y: position.y - 40 });
  };

  return (
    <div className="h-full w-full" onDrop={handleDrop} onDragOver={(event) => event.preventDefault()}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onConnect={onConnect}
        nodeTypes={nodeTypes}
        onNodeClick={(_, node) => onSelect(node as PipelineNode)}
        onPaneClick={() => onSelect(null)}
        fitView
        fitViewOptions={{ padding: 0.3 }}
        defaultEdgeOptions={{ animated: true, style: { stroke: "#38e8c6", strokeWidth: 1.4 } }}
        proOptions={{ hideAttribution: true }}
      >
        <Background variant={BackgroundVariant.Dots} gap={22} size={1} color="#1e293b" />
        <Controls className="!bottom-5 !left-5" />
        <MiniMap
          className="!bottom-5 !right-5 !bg-ink-900"
          maskColor="rgba(7,11,18,0.7)"
          nodeColor="#1f9e88"
        />
      </ReactFlow>
    </div>
  );
}
