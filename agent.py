# tiny-agent: 基于 Anthropic Messages API 的最小 agent 示例（tool_use/tool_result 循环）
#
# 整体结构（从上到下 4 块）：
#   1. 工具函数（get_color / get_number）     —— Agent 能调用的"能力"，模拟真实工具
#   2. 工具转换层（make_schemas / make_function_map）
#                                            —— 把 Python 函数变成 API 认识的工具说明书
#   3. MinCore 类                            —— 核心循环：发消息 -> 模型要调工具 ->
#                                              执行工具 -> 回填结果 -> 再发消息，
#                                              直到模型给出最终文本回答
#   4. main                                  —— 命令行 REPL，一轮一轮对话
#                                              （history 即短期记忆，进程内有效）
import os                    # 读环境变量（API key / base_url / model 配置）
import json                  # 把工具返回的 dict/list 序列化成字符串，塞进 tool_result
import random                # 示例工具用：随机颜色 / 随机数字
from dotenv import load_dotenv                  # 从 .env 文件加载环境变量
from anthropic import Anthropic                 # Anthropic 官方 SDK：调 Messages API
from function_schema import get_function_schema  # 从函数签名自动生成工具 schema


# ============ 1. 工具函数：Agent 的能力集 ============

def get_color() -> str:
    """示例工具：随机返回一个颜色。模拟 Agent 可以调用的外部能力。"""
    colors = ["red", "green", "blue", "yellow", "purple", "white", "black"]
    return random.choice(colors)


def get_number() -> int:
    """示例工具：随机返回 0-100 的整数。"""
    return random.randint(0, 100)


# ============ 2. 工具转换层：Python 函数 <-> API 工具说明书 ============

def make_schemas(funcs):
    """把函数列表转成 Anthropic 工具说明书（name/description/input_schema）。

    API 要求每个工具是 {name, description, input_schema} 的结构，
    模型读到这些说明才知道"有哪些工具可用、每个工具要什么参数"。
    """
    return [get_function_schema(f) for f in funcs]


def make_function_map(funcs):
    """函数名 -> 函数对象的映射表。

    模型返回的 tool_use 里只有工具名字符串（b.name），
    要用这个名字找到真正要执行的 Python 函数。
    """
    return {f.__name__: f for f in funcs}


# ============ 3. MinCore：Agent 核心（tool_use / tool_result 循环） ============

class MinCore:
    """最小 Agent 核心。

    职责：接收用户消息，自动执行"模型要工具 -> 执行 -> 回填结果"的循环，
    直到模型给出最终文本回答。对话历史（messages）由调用方持有并传回。
    """

    def __init__(self, api_key, base_url, model,
                 system_prompt="You are a helpful assistant."):
        """初始化：
        - client：Anthropic SDK 客户端（base_url 指向腾讯 TokenHub 中转端点）
        - model：模型名（deepseek-v4-flash-202605）
        - system_prompt：系统提示词，固定背景指令（每轮请求都会带上）
        """
        self.client = Anthropic(api_key=api_key, base_url=base_url)
        self.model = model
        self.system_prompt = system_prompt
        self.max_tokens = 2048  # 单次回答的最大 token 数（防止无限生成）

    def send_message(self, user_message, history=None, funcs=(), max_rounds=5):
        """发一条用户消息，自动执行工具调用循环。

        参数：
        - user_message：用户本轮输入
        - history：之前的对话历史（短期记忆！），None 表示新会话
        - funcs：本次可用的工具函数元组
        - max_rounds：工具循环的最大轮数（防止模型无限调工具）

        返回：(更新后的历史, 最终文本)
        - 调用方要把"更新后的历史"存好，下次再传回来，任务状态才延续
        """
        # --- 组装本次请求的工具说明 ---
        schemas = make_schemas(funcs) if funcs else None  # 有工具才生成 schema
        fn_map = make_function_map(funcs)                 # 名字 -> 函数

        # --- 短期记忆写入（1/3）：追加用户消息 ---
        # messages 就是短期记忆：记录"这次任务从头到尾发生了什么"
        # history 为 None 表示新会话，从空列表开始
        messages = history if history is not None else []
        messages.append({"role": "user", "content": user_message})  # 用户输入

        # --- 第一次调用模型 ---
        # 把"完整历史 + 系统提示词 + 工具说明"一次性发给模型
        response = self.client.messages.create(
            model=self.model,
            max_tokens=self.max_tokens,
            system=self.system_prompt,  # 固定背景指令
            messages=messages,          # 完整对话历史（短期记忆的读取）
            tools=schemas,              # 工具说明书，模型据此决定要不要调工具
        )

        # --- 工具循环：最多 max_rounds 轮 ---
        # 每轮：模型输出 tool_use -> 执行工具 -> 回填 tool_result -> 再问模型
        for _ in range(max_rounds):
            # 1) 看模型这轮输出里有没有工具调用请求
            tool_uses = [b for b in response.content if b.type == "tool_use"]
            if not tool_uses:
                break  # 模型没要求调工具 -> 准备直接回答了，退出循环

            # 2) 短期记忆写入（2/3）：助手这轮完整内容必须原样放回历史
            # 注意放的是 response.content（含 tool_use 块），不是只放文本！
            # 因为模型下一轮需要看到"自己上一步要求调了哪些工具"
            messages.append({"role": "assistant", "content": response.content})

            # 3) 逐个执行模型请求的工具
            results = []
            for b in tool_uses:
                fn = fn_map.get(b.name)  # 按工具名找到真实函数
                if fn is None:
                    # 模型要求了不存在的工具（幻觉/名字写错）——给个明确报错
                    output = f"unknown function: {b.name}"
                else:
                    result = fn(**b.input)  # 用模型给的参数调用工具
                    # 工具返回值要转成字符串：dict/list 用 json 序列化，
                    # 其他类型直接 str，保证能塞进 tool_result 的 content 字段
                    output = (json.dumps(result, default=str)
                              if isinstance(result, (dict, list)) else str(result))
                # 每个工具调用都要有对应的 tool_result，用 tool_use_id 关联
                results.append({
                    "type": "tool_result",
                    "tool_use_id": b.id,  # 必须对上模型给的 tool_use 的 id
                    "content": output,    # 工具执行结果（字符串）
                })

            # 4) 短期记忆写入（3/3）：工具结果回填
            # Anthropic API 规定：tool_result 消息的 role 必须写 "user"
            messages.append({"role": "user", "content": results})

            # 5) 带着"助手的要求 + 工具结果"再次问模型
            # 模型读到结果后：要么继续调工具，要么给出最终回答
            response = self.client.messages.create(
                model=self.model,
                max_tokens=self.max_tokens,
                system=self.system_prompt,
                messages=messages,
                tools=schemas,
            )
            # 回到循环顶部，再次检查有没有新的 tool_use

        # --- 收尾：提取最终文本回答 ---
        # 模型输出里可能有多个 text 块，拼接成一段字符串
        texts = [b.text for b in response.content if b.type == "text"]
        return messages, "\n".join(texts)  # 更新后的历史（短期记忆）也要返回


# ============ 4. main：命令行 REPL（对话入口） ============

def main():
    load_dotenv()  # 读 .env 里的配置
    api_key = os.getenv("BAZ_OPENAI_API_KEY")  # API key（腾讯 TokenHub 中转）
    base_url = os.getenv(                       # 中转端点
        "BAZ_OPENAI_BASE_URL",
        "https://api.lkeap.cloud.tencent.com/plan/anthropic")
    model = os.getenv("BAZ_OPENAI_MODEL", "deepseek-v4-flash-202605")
    if not api_key:
        raise ValueError("BAZ_OPENAI_API_KEY not set")

    # 创建 Agent：system prompt 告诉它自己是 Baz、有哪些工具、什么时候用
    llm = MinCore(api_key=api_key, base_url=base_url, model=model,
                  system_prompt=(
                      "You are Baz. You have two tools: get_color and get_number. "
                      "Use them when asked for colors or numbers."))
    funcs = (get_color, get_number)  # 注册可用工具

    # --- REPL 循环 ---
    # history 是短期记忆的"外部持有者"：每轮对话结束后把更新后的
    # messages 存回 history，下一轮再传进去，任务状态才得以延续。
    # 注意：它只活在进程内存里——退出程序就全部丢失，
    # 这正是下一步要加长期记忆的原因（对话精华落盘、跨会话可用）
    history = None
    while True:
        text = input("\n>>> You: ")
        # 传回 history（短期记忆），拿回更新后的 history 和回答
        history, reply = llm.send_message(text, history=history, funcs=funcs)
        print("\n>>> Agent:", reply)


if __name__ == "__main__":
    main()
