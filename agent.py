import random
def get_color()->str:
    colors=["red","green","blue","yellow","purple","white","black"]
    return random.choice(colors)
def get_number()->int:
    return random.randint(0,100)
from function_schema import get_function_schema
def make_schemas(funcs):
    schemas=[]
    for f in funcs:
        schema=get_function_schema(f)
        schema["type"]="function"
        schemas.append(schema)
    return schemas
def make_function_map(funcs):
    return{f._name_:f for f in funcs}
import json
def extract_calls(responce):
    calls=[]
    for item in getattr(responce,"output",None) or []:
        name =getattr(item,"name",None)
        args =getattr(item,"arguments",None)
        if not name or args is None:
            continue
        if isinstance(args,str): 
            try:
                args = json.loads(args)
            except Exception:
                args =[]
        calls=append({"id":getattr(item,"call.id","") or "","name":name,"agrs":args})
    return calls
