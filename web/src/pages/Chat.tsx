import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { chatStream, getConversation, listConversations, submitFeedback } from "../api/client";
import type { ChatMessage, Citation, ConversationSummary, IntentInfo } from "../types";
import { Button, EmptyState, PageHeader, Spinner } from "../components/ui";

type Phase = "idle" | "intent" | "retrieve" | "generate" | "done";

const INTENT_LABELS: Record<string, string> = {
  knowledge_qa: "知识问答",
  chitchat: "闲聊",
  clarify: "需澄清",
};

const STREAMING_ID = "__streaming__";

function citationAnchor(messageId: string, index: number): string {
  return `cite-${messageId}-${index}`;
}

/** Renders [n] markers in the answer as buttons that jump to the source card. */
function renderInlineCitations(
  text: string,
  citations: Citation[] | undefined,
  messageId: string,
  onJump: (anchor: string) => void,
): ReactNode[] {
  const parts = text.split(/(\[\d+\])/g);
  return parts.map((part, i) => {
    const match = /^\[(\d+)\]$/.exec(part);
    if (!match) return <span key={i}>{part}</span>;
    const n = Number(match[1]);
    const cite = citations?.[n - 1];
    if (!cite) return <span key={i}>{part}</span>;
    const anchor = citationAnchor(messageId, n - 1);
    return (
      <button
        key={i}
        type="button"
        onClick={() => onJump(anchor)}
        title={cite.meta?.file_name ? `跳转到来源：${cite.meta.file_name}` : `跳转到来源 ${n}`}
        className="mx-0.5 inline-flex h-4 min-w-4 items-center justify-center rounded bg-accent/15 px-1 align-super text-[9px] font-medium text-accent transition hover:bg-accent/30"
      >
        {n}
      </button>
    );
  });
}

function StageChip({
  label,
  state,
}: {
  label: string;
  state: "pending" | "active" | "done" | "skipped";
}) {
  const cls =
    state === "active"
      ? "border-accent/40 bg-accent/10 text-accent"
      : state === "done"
        ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-300"
        : state === "skipped"
          ? "border-white/10 bg-transparent text-slate-600 line-through"
          : "border-white/10 text-slate-500";
  return (
    <span className={`rounded-full border px-2 py-0.5 text-[9px] transition ${cls}`}>{label}</span>
  );
}

export default function Chat() {
  const [conversations, setConversations] = useState<ConversationSummary[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [loading, setLoading] = useState(false);
  const [phase, setPhase] = useState<Phase>("idle");
  const [intentInfo, setIntentInfo] = useState<IntentInfo | null>(null);
  const [stageMessageId, setStageMessageId] = useState<string | null>(null);
  const [highlight, setHighlight] = useState<string | null>(null);
  const [feedback, setFeedback] = useState<Record<string, number>>({});
  // Tracks which message is showing the "corrected answer" editor and which
  // messages already received a correction (upserts over the negative rating).
  const [correctingId, setCorrectingId] = useState<string | null>(null);
  const [correction, setCorrection] = useState("");
  const [corrected, setCorrected] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<string | null>(null);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  const refreshConversations = useCallback(async () => {
    try {
      const res = await listConversations();
      setConversations(res.conversations);
    } catch {
      // Listing is best-effort; chat errors are surfaced separately.
    }
  }, []);

  useEffect(() => {
    void refreshConversations();
  }, [refreshConversations]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "auto" });
  }, [messages, sending]);

  const resetStage = () => {
    setPhase("idle");
    setIntentInfo(null);
    setStageMessageId(null);
  };

  const jumpTo = (anchor: string, msg: ChatMessage) => {
    const el = document.getElementById(anchor);
    if (!el) return;
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    setHighlight(anchor);
    window.setTimeout(() => setHighlight((cur) => (cur === anchor ? null : cur)), 1800);
    if (!msg.id.startsWith("local-") && !msg.id.startsWith(STREAMING_ID)) {
      void submitFeedback({
        trace_id: msg.trace_id,
        message_id: msg.id,
        conversation_id: activeId ?? undefined,
        name: "user.citation_click",
        value: 1,
        meta: { anchor },
      }).catch(() => {
        // Implicit signals are best-effort.
      });
    }
  };

  const feedbackBase = (msg: ChatMessage) => ({
    trace_id: msg.trace_id,
    message_id: msg.id,
    conversation_id: activeId ?? undefined,
    query: msg.query,
    answer: msg.content,
    cited_chunk_ids: msg.citations?.map((c) => c.chunk_id) ?? [],
    meta: msg.intent ? { intent: msg.intent } : undefined,
  });

  const rate = async (msg: ChatMessage, value: number) => {
    if (feedback[msg.id] === value) return;
    const prev = feedback[msg.id];
    setFeedback((cur) => ({ ...cur, [msg.id]: value }));
    try {
      await submitFeedback({ ...feedbackBase(msg), name: "user.rating", value });
    } catch (e) {
      setFeedback((cur) => {
        const next = { ...cur };
        if (prev === undefined) delete next[msg.id];
        else next[msg.id] = prev;
        return next;
      });
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const submitCorrection = async (msg: ChatMessage) => {
    const text = correction.trim();
    if (!text) {
      setCorrectingId(null);
      return;
    }
    try {
      // Same subject/name as the rating, so this upserts instead of adding a row.
      await submitFeedback({
        ...feedbackBase(msg),
        name: "user.rating",
        value: -1,
        corrected_answer: text,
      });
      setCorrected((cur) => ({ ...cur, [msg.id]: true }));
      setCorrectingId(null);
      setCorrection("");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const openConversation = async (id: string) => {
    if (id === activeId) return;
    setActiveId(id);
    setCorrectingId(null);
    setCorrection("");
    setError(null);
    resetStage();
    setLoading(true);
    try {
      const detail = await getConversation(id);
      // Enrich assistant turns with their question so feedback keeps the query
      // even when the rating is submitted from history.
      const enriched = detail.messages.map((m, i) => {
        if (m.role !== "assistant" || i === 0) return m;
        const prev = detail.messages[i - 1];
        return prev.role === "user" ? { ...m, query: prev.content } : m;
      });
      setMessages(enriched);
    } catch (e) {
      setMessages([]);
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };

  const newChat = () => {
    setActiveId(null);
    setMessages([]);
    setCorrectingId(null);
    setCorrection("");
    setError(null);
    resetStage();
  };

  const send = async () => {
    const text = input.trim();
    if (!text || sending) return;

    const streamId = `${STREAMING_ID}-${Date.now()}`;
    const userId = `local-${Date.now()}`;
    const now = new Date().toISOString();
    setSending(true);
    setError(null);
    setPhase("intent");
    setIntentInfo(null);
    setStageMessageId(streamId);
    setInput("");
    setMessages((prev) => [
      ...prev,
      { id: userId, role: "user", content: text, created_at: now },
      {
        id: streamId,
        role: "assistant",
        content: "",
        citations: [],
        query: text,
        created_at: now,
      },
    ]);

    let answer = "";
    let intentLabel = "";
    try {
      await chatStream(text, activeId ?? undefined, {
        onIntent: (info) => {
          intentLabel = info.intent;
          setIntentInfo(info);
          setPhase(info.need_retrieval ? "retrieve" : "generate");
        },
        onStage: (stage, status) => {
          if (stage === "retrieve" && status === "start") setPhase("retrieve");
          if (stage === "generate") setPhase("generate");
        },
        onSources: (citations) => {
          setMessages((prev) =>
            prev.map((m) => (m.id === streamId ? { ...m, citations } : m)),
          );
        },
        onToken: (chunk) => {
          answer += chunk;
          setMessages((prev) =>
            prev.map((m) => (m.id === streamId ? { ...m, content: answer } : m)),
          );
        },
        onDone: (res) => {
          setActiveId(res.conversation_id);
          setMessages((prev) =>
            prev.map((m) =>
              m.id === streamId
                ? {
                    ...m,
                    id: res.message_id,
                    content: res.answer,
                    citations: res.citations,
                    trace_id: res.trace_id,
                    intent: intentLabel,
                    created_at: res.created_at,
                  }
                : m,
            ),
          );
          setStageMessageId(res.message_id);
          setPhase("done");
          void refreshConversations();
        },
      });
    } catch (e) {
      setMessages((prev) => prev.filter((m) => m.id !== streamId));
      setInput(text);
      setStageMessageId(null);
      setIntentInfo(null);
      setPhase("idle");
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSending(false);
    }
  };

  const stageState = (
    target: "intent" | "retrieve" | "generate" | "answer",
  ): "pending" | "active" | "done" | "skipped" => {
    const retrievalSkipped = Boolean(intentInfo && !intentInfo.need_retrieval);
    if (phase === "done") {
      if (target === "retrieve" && retrievalSkipped) return "skipped";
      return "done";
    }
    if (phase === "intent") return target === "intent" ? "active" : "pending";
    if (target === "intent") return "done";
    if (phase === "retrieve") {
      if (target === "retrieve") return "active";
      return "pending";
    }
    if (phase === "generate") {
      if (target === "retrieve") return retrievalSkipped ? "skipped" : "done";
      return target === "generate" ? "active" : "pending";
    }
    return "pending";
  };

  return (
    <div className="flex h-[calc(100vh-7.5rem)] flex-col">
      <PageHeader
        title="智能问答"
        description="多轮 RAG 问答：过程节点、检索知识、正文引用与一键跳转"
      />

      <div className="flex min-h-0 flex-1 gap-4">
        <aside className="panel flex w-60 shrink-0 flex-col p-3">
          <Button onClick={newChat} className="w-full">
            新对话
          </Button>
          <div className="mt-3 flex-1 space-y-1 overflow-auto">
            {conversations.length === 0 ? (
              <p className="px-1 py-3 text-[10px] text-slate-500">暂无历史会话</p>
            ) : (
              conversations.map((conv) => (
                <button
                  key={conv.id}
                  type="button"
                  onClick={() => void openConversation(conv.id)}
                  className={`w-full rounded-lg px-2.5 py-2 text-left transition ${
                    conv.id === activeId
                      ? "bg-accent/10 text-accent"
                      : "text-slate-300 hover:bg-white/5"
                  }`}
                >
                  <p className="truncate text-xs">{conv.title}</p>
                  <p className="mt-0.5 text-[9px] text-slate-500">
                    {conv.message_count} 条消息
                  </p>
                </button>
              ))
            )}
          </div>
        </aside>

        <section className="panel flex min-h-0 flex-1 flex-col">
          <div className="min-h-0 flex-1 space-y-4 overflow-auto p-4">
            {loading ? (
              <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
                <Spinner /> 加载会话…
              </div>
            ) : messages.length === 0 ? (
              <EmptyState
                title="开始提问"
                hint="例如：NexusFlow 的检索链路是如何工作的？回答会标注 [n] 引用，点击可跳转到对应知识片段。"
              />
            ) : (
              messages.map((msg) => {
                const isAssistant = msg.role === "assistant";
                const showStage = isAssistant && msg.id === stageMessageId && phase !== "idle";
                return (
                  <div
                    key={msg.id}
                    className={`flex ${isAssistant ? "justify-start" : "justify-end"}`}
                  >
                    <div
                      className={`max-w-[82%] rounded-xl px-3.5 py-2.5 text-xs leading-relaxed ${
                        isAssistant
                          ? "border border-white/5 bg-white/[0.03] text-slate-200"
                          : "bg-accent/15 text-slate-100"
                      }`}
                    >
                      {showStage && (
                        <div className="mb-2 space-y-1 border-b border-white/5 pb-2">
                          <div className="flex flex-wrap items-center gap-1.5">
                            <StageChip label="意图" state={stageState("intent")} />
                            <span className="text-slate-600">→</span>
                            <StageChip
                              label={
                                intentInfo && !intentInfo.need_retrieval
                                  ? "检索（跳过）"
                                  : "检索"
                              }
                              state={stageState("retrieve")}
                            />
                            <span className="text-slate-600">→</span>
                            <StageChip label="生成" state={stageState("generate")} />
                            <span className="text-slate-600">→</span>
                            <StageChip label="回答" state={stageState("answer")} />
                            {sending && (
                              <span className="ml-1 text-[9px] text-slate-500">进行中…</span>
                            )}
                          </div>
                          {intentInfo && (
                            <p className="truncate text-[9px] text-slate-500" title={intentInfo.reason}>
                              意图：{INTENT_LABELS[intentInfo.intent] ?? intentInfo.intent} ·
                              置信度 {intentInfo.confidence.toFixed(2)}
                              {intentInfo.reason ? ` · ${intentInfo.reason}` : ""}
                            </p>
                          )}
                        </div>
                      )}

                      {isAssistant ? (
                        <div className="whitespace-pre-wrap">
                          {renderInlineCitations(msg.content, msg.citations, msg.id, (anchor) =>
                            jumpTo(anchor, msg),
                          )}
                          {sending && msg.id === stageMessageId && !msg.content && (
                            <span className="text-slate-500">正在生成…</span>
                          )}
                        </div>
                      ) : (
                        <div className="whitespace-pre-wrap">{msg.content}</div>
                      )}

                      {isAssistant && (msg.citations?.length ?? 0) > 0 && (
                        <div className="mt-2.5 space-y-1 border-t border-white/5 pt-2">
                          <p className="text-[9px] uppercase tracking-wider text-slate-500">
                            检索到的知识 · {msg.citations?.length} 条
                          </p>
                          {msg.citations?.map((c, idx) => {
                            const anchor = citationAnchor(msg.id, idx);
                            return (
                              <div
                                key={`${c.chunk_id}-${idx}`}
                                id={anchor}
                                className={`rounded-lg border p-2 transition ${
                                  highlight === anchor
                                    ? "border-accent/60 bg-accent/10"
                                    : "border-white/5 bg-white/[0.02]"
                                }`}
                              >
                                <div className="flex flex-wrap items-center gap-1.5">
                                  <span className="rounded bg-accent/15 px-1 py-0.5 text-[9px] text-accent">
                                    [{idx + 1}]
                                  </span>
                                  {c.meta?.file_name && (
                                    <span
                                      className="truncate text-[9px] text-slate-400"
                                      title={c.meta.file_name}
                                    >
                                      文件：{c.meta.file_name}
                                    </span>
                                  )}
                                  <span className="ml-auto text-[9px] font-mono text-slate-500">
                                    score {c.score.toFixed(3)}
                                  </span>
                                </div>
                                <p className="mt-1 whitespace-pre-wrap text-[10px] leading-relaxed text-slate-400">
                                  {c.content}
                                </p>
                              </div>
                            );
                          })}
                        </div>
                      )}
                      {isAssistant && !sending && msg.content && !msg.id.startsWith(STREAMING_ID) && (
                        <>
                          <div className="mt-2 flex items-center gap-2 border-t border-white/5 pt-2">
                            <span className="text-[9px] text-slate-500">这个回答：</span>
                            <button
                              type="button"
                              onClick={() => {
                                setCorrectingId(null);
                                void rate(msg, 1);
                              }}
                              className={`rounded-full border px-2 py-0.5 text-[9px] transition ${
                                feedback[msg.id] === 1
                                  ? "border-emerald-500/40 bg-emerald-500/10 text-emerald-300"
                                  : "border-white/10 text-slate-400 hover:bg-white/5"
                              }`}
                            >
                              有帮助
                            </button>
                            <button
                              type="button"
                              onClick={() => {
                                void rate(msg, -1);
                                setCorrection("");
                                setCorrectingId(msg.id);
                              }}
                              className={`rounded-full border px-2 py-0.5 text-[9px] transition ${
                                feedback[msg.id] === -1
                                  ? "border-rose-500/40 bg-rose-500/10 text-rose-300"
                                  : "border-white/10 text-slate-400 hover:bg-white/5"
                              }`}
                            >
                              没帮助
                            </button>
                            {feedback[msg.id] !== undefined && (
                              <span className="text-[9px] text-slate-500">已记录</span>
                            )}
                            {corrected[msg.id] && (
                              <span className="text-[9px] text-emerald-400/70">已提交纠正</span>
                            )}
                          </div>
                          {correctingId === msg.id && (
                            <div className="mt-2 rounded-lg border border-white/10 bg-white/[0.02] p-2">
                              <textarea
                                value={correction}
                                onChange={(e) => setCorrection(e.target.value)}
                                rows={2}
                                autoFocus
                                placeholder="可选：填写你认为正确的答案，帮助改进检索与生成"
                                className="w-full resize-none rounded-md border border-white/10 bg-transparent px-2 py-1 text-[11px] text-slate-200 placeholder:text-slate-600 focus:border-sky-500/40 focus:outline-none"
                              />
                              <div className="mt-1 flex items-center justify-end gap-2">
                                <button
                                  type="button"
                                  onClick={() => {
                                    setCorrectingId(null);
                                    setCorrection("");
                                  }}
                                  className="rounded-full border border-white/10 px-2 py-0.5 text-[9px] text-slate-400 hover:bg-white/5"
                                >
                                  跳过
                                </button>
                                <button
                                  type="button"
                                  disabled={!correction.trim()}
                                  onClick={() => void submitCorrection(msg)}
                                  className="rounded-full border border-sky-500/40 bg-sky-500/10 px-2 py-0.5 text-[9px] text-sky-300 transition hover:bg-sky-500/20 disabled:opacity-40"
                                >
                                  提交纠正
                                </button>
                              </div>
                            </div>
                          )}
                        </>
                      )}
                    </div>
                  </div>
                );
              })
            )}
            {sending && (
              <div className="flex items-center gap-2 text-[10px] text-slate-500">
                <Spinner className="h-3 w-3" /> 正在检索并生成回答…
              </div>
            )}
            <div ref={bottomRef} />
          </div>

          {error && <div className="px-4 pb-2 text-xs text-rose-300">请求失败：{error}</div>}

          <div className="flex items-end gap-2 border-t border-white/5 p-3">
            <textarea
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  void send();
                }
              }}
              rows={2}
              placeholder="输入问题，Enter 发送，Shift+Enter 换行"
              className="flex-1 resize-none rounded-lg border border-white/10 bg-ink-950 px-3 py-2 text-xs text-slate-200 outline-none focus:border-accent/40"
            />
            <Button onClick={() => void send()} disabled={sending || !input.trim()}>
              {sending ? <Spinner /> : "发送"}
            </Button>
          </div>
        </section>
      </div>
    </div>
  );
}
