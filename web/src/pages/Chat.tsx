import { useCallback, useEffect, useRef, useState } from "react";
import { chat, getConversation, listConversations } from "../api/client";
import type { ChatMessage, ConversationSummary } from "../types";
import { Button, EmptyState, PageHeader, Spinner } from "../components/ui";

export default function Chat() {
  const [conversations, setConversations] = useState<ConversationSummary[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  const refreshConversations = useCallback(async () => {
    try {
      const res = await listConversations();
      setConversations(res.conversations);
    } catch {
      // Listing is best-effort; the chat itself surfaces errors.
    }
  }, []);

  useEffect(() => {
    void refreshConversations();
  }, [refreshConversations]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages, sending]);

  const openConversation = async (id: string) => {
    if (id === activeId) return;
    setActiveId(id);
    setError(null);
    setLoading(true);
    try {
      const detail = await getConversation(id);
      setMessages(detail.messages);
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
    setError(null);
  };

  const send = async () => {
    const text = input.trim();
    if (!text || sending) return;

    const draft: ChatMessage = {
      id: `local-${Date.now()}`,
      role: "user",
      content: text,
      created_at: new Date().toISOString(),
    };
    setSending(true);
    setError(null);
    setMessages((prev) => [...prev, draft]);
    setInput("");

    try {
      const res = await chat(text, activeId ?? undefined);
      setActiveId(res.conversation_id);
      setMessages((prev) => [
        ...prev,
        {
          id: res.message_id,
          role: "assistant",
          content: res.answer,
          citations: res.citations,
          created_at: res.created_at,
        },
      ]);
      void refreshConversations();
    } catch (e) {
      setMessages((prev) => prev.filter((m) => m.id !== draft.id));
      setInput(text);
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="flex h-[calc(100vh-7.5rem)] flex-col">
      <PageHeader
        title="智能问答"
        description="基于知识库的多轮 RAG 问答，答案附带检索引用，会话按用户隔离"
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
          <div className="min-h-0 flex-1 space-y-3 overflow-auto p-4">
            {loading ? (
              <div className="flex items-center gap-2 py-6 text-xs text-slate-500">
                <Spinner /> 加载会话…
              </div>
            ) : messages.length === 0 ? (
              <EmptyState
                title="开始提问"
                hint="例如：NexusFlow 的检索链路是如何工作的？答案会引用检索到的片段。"
              />
            ) : (
              messages.map((msg) => (
                <div
                  key={msg.id}
                  className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"}`}
                >
                  <div
                    className={`max-w-[82%] rounded-xl px-3.5 py-2.5 text-xs leading-relaxed ${
                      msg.role === "user"
                        ? "bg-accent/15 text-slate-100"
                        : "border border-white/5 bg-white/[0.03] text-slate-200"
                    }`}
                  >
                    <div className="whitespace-pre-wrap">{msg.content}</div>
                    {msg.role === "assistant" && msg.citations && msg.citations.length > 0 && (
                      <div className="mt-2 space-y-1 border-t border-white/5 pt-2">
                        <p className="text-[9px] uppercase tracking-wider text-slate-500">
                          引用 {msg.citations.length} 条
                        </p>
                        {msg.citations.map((c, idx) => (
                          <div key={`${c.chunk_id}-${idx}`} className="rounded bg-white/[0.03] p-1.5">
                            <span className="mr-1 text-[9px] text-accent">[{idx + 1}]</span>
                            <span className="text-[10px] text-slate-400">{c.content}</span>
                            {c.meta?.file_name && (
                              <span className="ml-1 text-[9px] text-slate-500">· {c.meta.file_name}</span>
                            )}
                          </div>
                        ))}
                      </div>
                    )}
                  </div>
                </div>
              ))
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
