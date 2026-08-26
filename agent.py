# tiny-agent: 基于 Anthropic Messages API 的最小 agent 示例（tool_use/tool_result 循环）
import os
import json
import random
from dotenv import load_dotenv
from anthropic import Anthropic
from function_schema import get_function_schema


def get_color() -> str:
    colors = ["red", "green", "blue", "yellow", "purple", "white", "black"]
    return random.choice(colors)


def get_number() -> int:
    return random.randint(0, 100)


def make_schemas(funcs):
    """把函数列表转成 Anthropic 工具说明书（name/description/input_schema）。"""
    return [get_function_schema(f) for f in funcs]


def make_function_map(funcs):
    return {f.__name__: f for f in funcs}


class MinCore:
    def __init__(self, api_key, base_url, model,
                 system_prompt="You are a helpful assistant."):
        self.client = Anthropic(api_key=api_key, base_url=base_url)
        self.model = model
        self.system_prompt = system_prompt
        self.max_tokens = 2048

    def send_message(self, user_message, history=None, funcs=(), max_rounds=5):
        """发一条用户消息，自动执行工具调用循环，返回 (更新后的历史, 最终文本)。"""
        schemas = make_schemas(funcs) if funcs else None
        fn_map = make_function_map(funcs)

        messages = history if history is not None else []
        messages.append({"role": "user", "content": user_message})

        response = self.client.messages.create(
            model=self.model,
            max_tokens=self.max_tokens,
            system=self.system_prompt,
            messages=messages,
            tools=schemas,
        )

        for _ in range(max_rounds):
            tool_uses = [b for b in response.content if b.type == "tool_use"]
            if not tool_uses:
                break

            # 助手这一轮的完整内容（含 tool_use）必须原样放回历史
            messages.append({"role": "assistant", "content": response.content})

            results = []
            for b in tool_uses:
                fn = fn_map.get(b.name)
                if fn is None:
                    output = f"unknown function: {b.name}"
                else:
                    result = fn(**b.input)  # 执行工具函数
                    output = (json.dumps(result, default=str)
                              if isinstance(result, (dict, list)) else str(result))
                results.append({
                    "type": "tool_result",
                    "tool_use_id": b.id,
                    "content": output,
                })
            messages.append({"role": "user", "content": results})

            response = self.client.messages.create(
                model=self.model,
                max_tokens=self.max_tokens,
                system=self.system_prompt,
                messages=messages,
                tools=schemas,
            )

        texts = [b.text for b in response.content if b.type == "text"]
        return messages, "\n".join(texts)


def main():
    load_dotenv()
    api_key = os.getenv("BAZ_OPENAI_API_KEY")
    base_url = os.getenv(
        "BAZ_OPENAI_BASE_URL",
        "https://api.lkeap.cloud.tencent.com/plan/anthropic")
    model = os.getenv("BAZ_OPENAI_MODEL", "deepseek-v4-flash-202605")
    if not api_key:
        raise ValueError("BAZ_OPENAI_API_KEY not set")

    llm = MinCore(api_key=api_key, base_url=base_url, model=model,
                  system_prompt=(
                      "You are Baz. You have two tools: get_color and get_number. "
                      "Use them when asked for colors or numbers."))
    funcs = (get_color, get_number)

    history = None
    while True:
        text = input("\n>>> You: ")
        history, reply = llm.send_message(text, history=history, funcs=funcs)
        print("\n>>> Agent:", reply)


if __name__ == "__main__":
    main()
