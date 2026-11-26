import json
import os
import threading
import unittest
from argparse import Namespace
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

import judge


class ParseScoresTest(unittest.TestCase):
    def test_plain_json(self):
        scores = judge.parse_scores(
            '{"faithfulness": 0.8, "relevancy": 0.6, "completeness": 0.5, "reason": "ok"}'
        )
        self.assertAlmostEqual(scores["faithfulness"], 0.8)
        self.assertAlmostEqual(scores["relevancy"], 0.6)
        self.assertAlmostEqual(scores["completeness"], 0.5)
        self.assertEqual(scores["reason"], "ok")

    def test_code_fence_and_prose_are_tolerated(self):
        text = '说明如下：\n```json\n{"faithfulness": 1, "relevancy": 1, "completeness": 1}\n```\n'
        scores = judge.parse_scores(text)
        self.assertEqual(scores["faithfulness"], 1.0)
        self.assertEqual(scores["reason"], "")

    def test_out_of_range_values_are_clamped(self):
        scores = judge.parse_scores(
            '{"faithfulness": 1.4, "relevancy": -0.2, "completeness": 0.5}'
        )
        self.assertEqual(scores["faithfulness"], 1.0)
        self.assertEqual(scores["relevancy"], 0.0)

    def test_missing_score_raises(self):
        with self.assertRaises(ValueError):
            judge.parse_scores('{"faithfulness": 0.8, "relevancy": 0.6}')

    def test_non_json_reply_raises(self):
        with self.assertRaises(ValueError):
            judge.parse_scores("抱歉，我无法评分")


class PromptTest(unittest.TestCase):
    def test_contexts_are_numbered(self):
        messages = judge.build_judge_messages("问题", "回答", ["片段一", "片段二"])
        self.assertEqual(messages[0]["role"], "system")
        user = messages[1]["content"]
        self.assertIn("问题", user)
        self.assertIn("回答", user)
        self.assertIn("[1] 片段一", user)
        self.assertIn("[2] 片段二", user)

    def test_missing_contexts_use_placeholder(self):
        user = judge.build_judge_messages("问题", "回答", [])[1]["content"]
        self.assertIn("无引用片段", user)


class AggregateTest(unittest.TestCase):
    def test_means_skip_errors(self):
        rows = [
            {"scores": {"faithfulness": 1.0, "relevancy": 0.5, "completeness": 0.5}},
            {"scores": {"faithfulness": 0.0, "relevancy": 0.5, "completeness": 0.5}},
            {"error": "boom"},
        ]
        metrics = judge.aggregate(rows)
        self.assertAlmostEqual(metrics["faithfulness"], 0.5)
        self.assertAlmostEqual(metrics["relevancy"], 0.5)
        self.assertEqual(metrics["judged"], 2.0)
        self.assertEqual(metrics["errors"], 1.0)

    def test_empty_rows_are_zero(self):
        metrics = judge.aggregate([])
        self.assertEqual(metrics["faithfulness"], 0.0)
        self.assertEqual(metrics["judged"], 0.0)


class ThresholdTest(unittest.TestCase):
    def test_reports_only_failed_metrics(self):
        metrics = {"faithfulness": 0.75, "relevancy": 0.5, "completeness": 0.9}
        failures = judge.check_thresholds(
            metrics, {"faithfulness": 0.7, "relevancy": 0.7, "missing": 0.9}
        )
        self.assertEqual(len(failures), 1)
        self.assertIn("relevancy", failures[0])


class LLMConfigTest(unittest.TestCase):
    def test_missing_key_disables_judge(self):
        args = Namespace(llm_base_url=None, llm_api_key=None, llm_model=None)
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertIsNone(judge.llm_config(args))

    def test_env_fills_config(self):
        args = Namespace(llm_base_url=None, llm_api_key=None, llm_model=None)
        env = {
            "RAG_LLM_BASE_URL": "https://example.test/v1",
            "RAG_LLM_API_KEY": "sk-test",
            "RAG_LLM_MODEL": "deepseek-chat",
        }
        with mock.patch.dict(os.environ, env, clear=True):
            cfg = judge.llm_config(args)
        self.assertEqual(cfg.llm_base_url, "https://example.test/v1")
        self.assertEqual(cfg.llm_api_key, "sk-test")
        self.assertEqual(cfg.llm_model, "deepseek-chat")


class _MockHandler(BaseHTTPRequestHandler):
    captured: dict = {}

    def do_POST(self):  # noqa: N802 - http.server API
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        type(self).captured = {
            "path": self.path,
            "auth": self.headers.get("Authorization"),
            "body": body,
        }
        content = '```json\n{"faithfulness": 0.9, "relevancy": 0.8, "completeness": 0.7, "reason": "grounded"}\n```'
        payload = json.dumps({"choices": [{"message": {"content": content}}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args):
        pass


class JudgeAnswerHTTPTest(unittest.TestCase):
    def test_calls_openai_compatible_endpoint(self):
        server = HTTPServer(("127.0.0.1", 0), _MockHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            cfg = Namespace(
                llm_base_url=f"http://127.0.0.1:{server.server_port}/v1",
                llm_api_key="sk-test",
                llm_model="test-model",
            )
            scores = judge.judge_answer(cfg, "问题", "回答", ["片段"], timeout=10)
        finally:
            server.shutdown()
            server.server_close()

        self.assertAlmostEqual(scores["faithfulness"], 0.9)
        captured = _MockHandler.captured
        self.assertEqual(captured["path"], "/v1/chat/completions")
        self.assertEqual(captured["auth"], "Bearer sk-test")
        self.assertEqual(captured["body"]["model"], "test-model")
        self.assertEqual(captured["body"]["messages"][0]["role"], "system")


if __name__ == "__main__":
    unittest.main()
