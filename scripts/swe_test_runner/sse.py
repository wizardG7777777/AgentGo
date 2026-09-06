"""SWE Test Runner 的有界 SSE 能力探针解码，不提供非流式降级。"""

import json


def events(response):
    """按 SSE 空行分隔事件，保留 UTF-8 和多行 data。"""
    data = []
    size = 0
    total = 0
    for line in response:
        if isinstance(line, bytes):
            line = line.decode("utf-8")
        line = line.rstrip("\r\n")
        size += len(line.encode("utf-8"))
        total += len(line.encode("utf-8"))
        if total > 4 * 1024 * 1024:
            raise RuntimeError("SSE 探针响应超过 4MiB")
        if size > 1024 * 1024:
            raise RuntimeError("SSE 探针事件超过 1MiB")
        if not line:
            if data:
                yield "\n".join(data)
            data, size = [], 0
        elif line.startswith("data:"):
            data.append(line[5:].removeprefix(" "))
    if data:
        raise RuntimeError("SSE 探针在事件结束前断流")


def decode_probe(response, protocol):
    """只在收到协议终止标记后返回可供 nonce 校验的完整结果。"""
    output = {}
    text = ""
    calls = {}
    finish = None
    for raw in events(response):
        if raw == "[DONE]":
            if protocol != "chat_completions" or finish not in {"stop", "tool_calls", "length", "content_filter"}:
                raise RuntimeError("SSE 探针缺少合法完成原因")
            return {"choices": [{"index": 0, "finish_reason": finish,
                                  "message": {"role": "assistant", "content": text,
                                              "tool_calls": [calls[k] for k in sorted(calls)]}}]}
        event = json.loads(raw)
        if not isinstance(event, dict):
            raise RuntimeError("SSE 探针事件不是对象")
        if event.get("error") or event.get("type") in {"error", "response.failed"}:
            raise RuntimeError("SSE 探针返回协议失败事件")
        if protocol == "responses":
            kind = event.get("type")
            if kind == "response.output_item.done":
                index = event.get("output_index")
                if not isinstance(index, int) or index < 0 or index in output:
                    raise RuntimeError("SSE 探针输出项身份无效或重复")
                output[index] = event.get("item")
            elif kind == "response.completed":
                result = event.get("response") or {}
                if result.get("status") != "completed" or not output:
                    raise RuntimeError("SSE 探针缺少完整输出项")
                if sorted(output) != list(range(len(output))):
                    raise RuntimeError("SSE 探针输出项序号不连续")
                return {**result, "output": [output[k] for k in sorted(output)]}
            elif kind == "response.incomplete":
                raise RuntimeError("SSE 探针响应被截断")
        elif protocol == "chat_completions":
            for choice in event.get("choices") or []:
                if choice.get("index", 0) != 0:
                    raise RuntimeError("SSE 探针只允许一个候选回复")
                delta = choice.get("delta") or {}
                text += delta.get("content") or ""
                for part in delta.get("tool_calls") or []:
                    index = part.get("index")
                    if not isinstance(index, int) or index < 0:
                        raise RuntimeError("SSE 探针工具序号无效")
                    call = calls.setdefault(index, {"id": "", "type": "function", "function": {"name": "", "arguments": ""}})
                    call["id"] += part.get("id") or ""
                    function = part.get("function") or {}
                    call["function"]["name"] += function.get("name") or ""
                    call["function"]["arguments"] += function.get("arguments") or ""
                if choice.get("finish_reason") is not None:
                    finish = choice["finish_reason"]
        else:
            raise RuntimeError("未知 SSE 探针协议")
    raise RuntimeError("SSE 探针未收到完整终止事件")
