from __future__ import annotations

from app.schemas import IntentResult

GREETINGS = (
    "你好",
    "您好",
    "hi",
    "hello",
    "hey",
    "在吗",
    "谢谢",
    "感谢",
    "再见",
    "bye",
)

ACTION_HINTS = (
    "帮我",
    "生成",
    "创建",
    "删除",
    "执行",
    "部署",
    "发送",
    "修改",
    "更新",
    "上传",
)

QUESTION_HINTS = (
    "?",
    "？",
    "怎么",
    "如何",
    "为什么",
    "是什么",
    "哪些",
    "多少",
    "是否",
    "能否",
    "介绍",
    "解释",
    "区别",
)


def classify_rules(message: str) -> IntentResult:
    """Deterministic fallback used when no LLM is configured or it fails."""
    text = message.strip()
    lowered = text.lower()
    has_question = any(hint in lowered for hint in QUESTION_HINTS)

    if not text:
        return IntentResult(
            intent="clarify",
            confidence=0.9,
            need_retrieval=False,
            reason="空消息，需要用户补充问题",
            source="rules",
        )

    if any(g in lowered for g in GREETINGS) and not has_question and len(text) <= 20:
        return IntentResult(
            intent="chitchat",
            confidence=0.8,
            need_retrieval=False,
            reason="检测到问候或寒暄",
            source="rules",
        )

    if any(a in text for a in ACTION_HINTS) and not has_question:
        return IntentResult(
            intent="clarify",
            confidence=0.65,
            need_retrieval=False,
            reason="检测到操作类请求，当前版本暂不支持，需向用户澄清",
            source="rules",
        )

    if len(text) <= 3 and not has_question:
        return IntentResult(
            intent="clarify",
            confidence=0.55,
            need_retrieval=False,
            reason="问题过短且意图不明确",
            source="rules",
        )

    return IntentResult(
        intent="knowledge_qa",
        confidence=0.7,
        need_retrieval=True,
        reason="默认按知识库问答处理",
        source="rules",
    )
