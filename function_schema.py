import inspect


def get_function_schema(f):
    """把 Python 函数翻译成 Anthropic 工具说明书（JSON Schema）。"""
    sig = inspect.signature(f)
    properties, required = {}, []
    for name, param in sig.parameters.items():
        ann = param.annotation
        t = {int: "integer", str: "string", float: "number", bool: "boolean"}.get(ann, "string")
        properties[name] = {"type": t}
        if param.default is inspect.Parameter.empty:
            required.append(name)
    return {
        "name": f.__name__,
        "description": f.__doc__ or f"Call {f.__name__}",
        "input_schema": {"type": "object", "properties": properties, "required": required},
    }
