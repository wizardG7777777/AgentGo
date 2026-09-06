import io
import json
import unittest

from sse import decode_probe, events


def frames(*values):
    return io.BytesIO(b"".join(("data: " + (v if isinstance(v, str) else json.dumps(v, ensure_ascii=False)) + "\r\n\r\n").encode("utf-8") for v in values))


class SSEProbeTests(unittest.TestCase):
    def test_chat_tool_arguments_are_assembled_before_done(self):
        result = decode_probe(frames(
            {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "id": "call-1", "function": {"name": "检查", "arguments": '{"nonce":'}}]}}]},
            {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"arguments": '"值"}'}}]}, "finish_reason": "tool_calls"}]},
            "[DONE]"), "chat_completions")
        call = result["choices"][0]["message"]["tool_calls"][0]
        self.assertEqual(call["function"]["arguments"], '{"nonce":"值"}')

    def test_chat_eof_does_not_replace_done(self):
        with self.assertRaisesRegex(RuntimeError, "终止"):
            decode_probe(frames({"choices": [{"index": 0, "delta": {"content": "未完整"}, "finish_reason": "stop"}]}), "chat_completions")

    def test_responses_requires_completed_and_unique_items(self):
        item = {"type": "function_call", "id": "item-1", "call_id": "call-1", "name": "检查", "arguments": "{}"}
        done = {"type": "response.output_item.done", "output_index": 0, "item": item}
        completed = {"type": "response.completed", "response": {"status": "completed"}}
        result = decode_probe(frames(done, completed), "responses")
        self.assertEqual(result["output"], [item])
        for stream in (frames(done), frames(done, done, completed), frames(completed)):
            with self.assertRaises(RuntimeError):
                decode_probe(stream, "responses")

    def test_multiline_data_and_comment(self):
        stream = io.BytesIO(b": heartbeat\r\ndata: first\r\ndata: second\r\n\r\n")
        self.assertEqual(list(events(stream)), ["first\nsecond"])


if __name__ == "__main__":
    unittest.main()
