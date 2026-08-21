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
        calls.append({"id":getattr(item,"call.id","") or "","name":name,"args":args})
    return calls
from openai import OpenAI
class Mincore:
    def _init_ (self,api_key,model="gpt-4o-mini",system_prompt="You are a helpful assistant."):
        self.client=OpenAI(api_key=api_key)
        self.model = model
        self.system_prompt = system_prompt
        #初始化
    def send_message(self,user_message,previous_response_id=None,funcs=(),max_rounds=5):
        #第一次对话，建立对话
        if previous_response_id is None:
            r=self.client.response.create(
                model:self.model
                input=[{"role":"system","content":self.system_prompt}]
            )
            previous_response_id=r.id
        schemas= make_schemas(funcs)
        fn_map=make_function_map(funcs)
        #发用户消息，带上说明书
        response=self.client.response.create(
            model=self.model,
            previous_response_id=previous_response_id,
            input=[{"role":"user","content":user_message}],
            tools=schemas if funcs else None,
        )
        for _ in range (max_rounds):
            calls=extract_calls(response)
            if not calls:
                break
            results=[]
            for c in calls:
                result=fn_map[c["name"]](**c["args"]) #执行工具函数
                output=json.dumps(result,default=str) if isinstance (result,(dict,list)) else str(result)
                results.append({"type": "function_call_output","call_id":c["id"],"output":output})
            response=self.client.responses.create(
                model=self.model,
                previous_response_id=response.id,
                input=results,
            )
            return response.id,response.output_text

    