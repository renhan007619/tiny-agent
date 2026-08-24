"""非交互测试：验证工具调用闭环（用完可删）。"""
import os
from dotenv import load_dotenv
from agent import MinCore, get_color, get_number

load_dotenv()
llm = MinCore(
    api_key=os.getenv("BAZ_OPENAI_API_KEY"),
    base_url=os.getenv("BAZ_OPENAI_BASE_URL",
                       "https://api.lkeap.cloud.tencent.com/plan/anthropic"),
    model=os.getenv("BAZ_OPENAI_MODEL", "deepseek-v4-flash-202605"),
    system_prompt=("You are Baz. You have two tools: get_color and get_number. "
                   "Use them when asked for colors or numbers."),
)
funcs = (get_color, get_number)

history = None
for q in ["give me a color", "now give me a color and a number"]:
    print("Q:", q)
    history, reply = llm.send_message(q, history=history, funcs=funcs)
    print("A:", reply)
    print("---")
