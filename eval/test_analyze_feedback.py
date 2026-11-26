import json
import tempfile
import unittest
from argparse import Namespace
from pathlib import Path

import analyze_feedback as af


def make_args(**overrides):
    base = {"base_url": "http://127.0.0.1:8080", "rating": -1.0, "limit": 200}
    base.update(overrides)
    return Namespace(**base)


def make_entry(**overrides):
    entry = {
        "subject_id": "msg-1",
        "message_id": "msg-1",
        "conversation_id": "conv-1",
        "trace_id": "trace-1",
        "rating": -1,
        "query": "q",
        "answer": "a",
        "cited_chunk_ids": [],
        "meta": {"intent": "knowledge_qa"},
        "created_at": "2026-01-01T00:00:00Z",
    }
    entry.update(overrides)
    return entry


class ClassifyFeedbackTest(unittest.TestCase):
    def test_corrected_answer_wins_over_other_signals(self):
        entry = make_entry(
            corrected_answer="正确答案",
            answer="未在知识库中找到相关内容",
            cited_chunk_ids=[],
        )
        self.assertEqual(af.classify_feedback(entry), af.GENERATION_WRONG)

    def test_blank_corrected_answer_is_ignored(self):
        entry = make_entry(corrected_answer="   ", cited_chunk_ids=["c1"], answer="见 [1]")
        self.assertEqual(af.classify_feedback(entry), af.UNCLASSIFIED)

    def test_missing_citations_is_retrieval_miss(self):
        entry = make_entry(answer="直接给了一个没有依据的答案", cited_chunk_ids=[])
        self.assertEqual(af.classify_feedback(entry), af.RETRIEVAL_MISS)

    def test_no_answer_phrase_is_retrieval_miss_even_with_citations(self):
        entry = make_entry(answer="资料不足，无法回答", cited_chunk_ids=["c1", "c2"])
        self.assertEqual(af.classify_feedback(entry), af.RETRIEVAL_MISS)

    def test_citations_without_markers_is_citation_missing(self):
        entry = make_entry(answer="这是一段没有角标的答案", cited_chunk_ids=["c1"])
        self.assertEqual(af.classify_feedback(entry), af.CITATION_MISSING)

    def test_marker_beyond_citation_count_is_out_of_range(self):
        entry = make_entry(answer="结论见 [3]", cited_chunk_ids=["c1", "c2"])
        self.assertEqual(af.classify_feedback(entry), af.CITATION_OUT_OF_RANGE)

    def test_in_range_marker_is_unclassified(self):
        entry = make_entry(answer="结论见 [1] 与 [2]", cited_chunk_ids=["c1", "c2"])
        self.assertEqual(af.classify_feedback(entry), af.UNCLASSIFIED)


class HelpersTest(unittest.TestCase):
    def test_marker_indexes_handles_multiple_and_missing(self):
        self.assertEqual(af.marker_indexes("见 [1] 和 [10]"), [1, 10])
        self.assertEqual(af.marker_indexes("没有角标"), [])

    def test_entry_intent_falls_back_to_unknown(self):
        self.assertEqual(af.entry_intent(make_entry()), "knowledge_qa")
        self.assertEqual(af.entry_intent(make_entry(meta=None)), "unknown")
        self.assertEqual(af.entry_intent(make_entry(meta={})), "unknown")


class SummarizeTest(unittest.TestCase):
    def test_aggregates_by_attribution_and_intent(self):
        entries = [
            make_entry(subject_id="m1", cited_chunk_ids=[], answer="没有找到相关内容"),
            make_entry(subject_id="m2", cited_chunk_ids=["c1"], answer="裸答案"),
            make_entry(
                subject_id="m3",
                meta={},
                cited_chunk_ids=["c1"],
                answer="见 [1]",
            ),
        ]
        report = af.summarize(entries, make_args())
        self.assertEqual(report["total"], 3)
        self.assertEqual(report["by_attribution"][af.RETRIEVAL_MISS], 1)
        self.assertEqual(report["by_attribution"][af.CITATION_MISSING], 1)
        self.assertEqual(report["by_attribution"][af.UNCLASSIFIED], 1)
        self.assertEqual(report["by_intent"]["knowledge_qa"][af.RETRIEVAL_MISS], 1)
        self.assertEqual(report["by_intent"]["unknown"][af.UNCLASSIFIED], 1)
        self.assertEqual(len(report["regressions"]), 3)
        self.assertEqual(report["regressions"][0]["attribution"], af.RETRIEVAL_MISS)


class OutputTest(unittest.TestCase):
    def test_writes_report_and_jsonl(self):
        with tempfile.TemporaryDirectory() as tmp:
            report_path = Path(tmp) / "report.json"
            out_path = Path(tmp) / "regressions.jsonl"
            report = af.summarize(
                [make_entry(cited_chunk_ids=[], answer="没有找到")], make_args()
            )
            af.write_outputs(report, report_path, out_path)

            written = json.loads(report_path.read_text(encoding="utf-8"))
            self.assertNotIn("regressions", written)
            self.assertEqual(written["total"], 1)

            lines = out_path.read_text(encoding="utf-8").splitlines()
            self.assertEqual(len(lines), 1)
            self.assertEqual(json.loads(lines[0])["attribution"], af.RETRIEVAL_MISS)

    def test_empty_feedback_writes_empty_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            report_path = Path(tmp) / "report.json"
            out_path = Path(tmp) / "regressions.jsonl"
            report = af.summarize([], make_args())
            af.write_outputs(report, report_path, out_path)
            written = json.loads(report_path.read_text(encoding="utf-8"))
            self.assertEqual(written["total"], 0)
            self.assertEqual(out_path.read_text(encoding="utf-8"), "")


if __name__ == "__main__":
    unittest.main()
