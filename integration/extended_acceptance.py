"""Opt-in real native plugin acceptance; all outputs contain synthetic data."""
import base64,hashlib,json,os,pathlib,secrets,subprocess,time,urllib.parse
from native_probe import NativeCPA,TASK
def parsed(text):
 value=text.strip()
 if value.startswith('```'):value=value.split('\n',1)[1].strip().removesuffix('```').strip()
 return json.loads(value)

FIXTURES=pathlib.Path(__file__).resolve().parent/'fixtures'
expected=json.loads((FIXTURES/'expected.json').read_text())
image='data:image/png;base64,'+base64.b64encode((FIXTURES/'fixture.png').read_bytes()).decode()
pdf='data:application/pdf;base64,'+base64.b64encode((FIXTURES/'fixture.pdf').read_bytes()).decode()
report={'success':False,'native_plugin':True,'cases':[]}
selected=set(os.environ.get('DOUBAO_CASES','stream_chat,stream_responses,file_id,responses_media,file_url,tool_media_chat,tool_media_responses').split(','))

def record(case,**facts):
 report['cases'].append(dict(case=case,success=True,**facts));print('VERIFIED',case,flush=True)

def stream(client,protocol):
 started=time.monotonic();parts=[];first=None;final=None
 prompt='请直接写一篇约500字的中文科普，介绍水循环，只写正文，不要调用工具。'
 if protocol=='chat':
  with client.chat.completions.create(model='doubao-work-pro',messages=[{'role':'user','content':prompt}],stream=True,extra_headers={'Idempotency-Key':'stream-'+secrets.token_hex(10)}) as result:
   for chunk in result:
    for choice in chunk.choices:
     if choice.delta.content:
      first=first or time.monotonic();parts.append(choice.delta.content)
     if choice.finish_reason:final=time.monotonic()
 else:
  with client.responses.create(model='doubao-work-pro',input=prompt,stream=True,extra_headers={'Idempotency-Key':'stream-'+secrets.token_hex(10)}) as result:
   for event in result:
    if event.type=='response.output_text.delta':first=first or time.monotonic();parts.append(event.delta)
    if event.type=='response.completed':
     final=time.monotonic();assert event.response.output_text==''.join(parts),'delta/final mismatch'
    if event.type in ('error','response.failed'):raise AssertionError(event.type)
 assert len(parts)>5 and first<final and len(''.join(parts))>100,(len(parts),first,final)
 record('stream_'+protocol,text_delta_count=len(parts),first_text_ms=round((first-started)*1000),completion_ms=round((final-started)*1000),final_sha256=hashlib.sha256(''.join(parts).encode()).hexdigest())

def media(client,protocol,file_id=None):
 prompt='请实际读取附带的图片和 PDF。只输出 JSON，字段 red_squares 为红色正方形数量，blue_circles 为蓝色圆形数量，code 为 PDF 中 Verification code，sum 为 Amount A 与 Amount B 之和。'
 if protocol=='chat':
  f={'file_id':file_id} if file_id else {'filename':'fixture.pdf','file_data':pdf}
  result=client.chat.completions.create(model='doubao-work-pro',messages=[{'role':'user','content':[{'type':'text','text':prompt},{'type':'image_url','image_url':{'url':image}},{'type':'file','file':f}]}])
  text=result.choices[0].message.content
 else:
  result=client.responses.create(model='doubao-work-pro',input=[{'role':'user','content':[{'type':'input_text','text':prompt},{'type':'input_image','image_url':image},{'type':'input_file','filename':'fixture.pdf','file_data':pdf}]}])
  text=result.output_text
 assert parsed(text)==expected,text
 record('file_id' if file_id else 'responses_media',exact_image_pdf_match=True)

def tool_media(client,protocol):
 challenge='assets_'+secrets.token_hex(10)
 prompt='请先真实调用 fetch_assets，challenge 为 '+challenge+'。图片和 PDF 只存在于工具结果中。取得后实际读取，只输出 JSON，字段 red_squares 为红色正方形数量，blue_circles 为蓝色圆形数量，code 为 PDF 中 Verification code，sum 为 Amount A 与 Amount B 之和。'
 function={'name':'fetch_assets','description':'Fetch an image and PDF as real content parts.','parameters':{'type':'object','properties':{'challenge':{'type':'string'}},'required':['challenge']}}
 if protocol=='chat':
  tools=[{'type':'function','function':function}];history=[{'role':'user','content':prompt}]
  first=client.chat.completions.create(model='doubao-work-pro',messages=history,tools=tools)
  assistant=first.choices[0].message
  assert len(assistant.tool_calls)==1 and json.loads(assistant.tool_calls[0].function.arguments)=={'challenge':challenge}
  history += [assistant.model_dump(exclude_none=True),{'role':'tool','tool_call_id':assistant.tool_calls[0].id,'content':[{'type':'text','text':'Requested assets:'},{'type':'image_url','image_url':{'url':image}},{'type':'file','file':{'filename':'fixture.pdf','file_data':pdf}}]}]
  result=client.chat.completions.create(model='doubao-work-pro',messages=history,tools=tools)
  text=result.choices[0].message.content
 else:
  tools=[{'type':'function',**function}];history=[{'role':'user','content':prompt}]
  first=client.responses.create(model='doubao-work-pro',input=history,tools=tools)
  calls=[x for x in first.output if x.type=='function_call'];assert len(calls)==1 and json.loads(calls[0].arguments)=={'challenge':challenge}
  history += [x.model_dump(exclude_none=True) for x in first.output]
  history += [{'type':'function_call_output','call_id':calls[0].call_id,'output':[{'type':'input_text','text':'Requested assets:'},{'type':'input_image','image_url':image},{'type':'input_file','filename':'fixture.pdf','file_data':pdf}]}]
  result=client.responses.create(model='doubao-work-pro',input=history,tools=tools)
  text=result.output_text
 assert parsed(text)==expected,text
 record('tool_media_'+protocol,real_function_called=True,image_pdf_from_tool_exact_match=True,hidden_values_absent_from_original_prompt=expected['code'] not in prompt)

try:
 with NativeCPA() as cpa,cpa.client() as client:
  for protocol in ('chat','responses'):
   if 'stream_'+protocol in selected:stream(client,protocol)
  if 'file_id' in selected:
   command=[str(TASK/'build/doubao-helper'),'-dir',str(TASK/'private')]
   added=json.loads(subprocess.check_output(command+['-action','file-put','-file',str(FIXTURES/'fixture.pdf')]))
   try:media(client,'chat',added['id'])
   finally:subprocess.check_output(command+['-action','file-delete','-file-id',added['id']])
  if 'responses_media' in selected:media(client,'responses')
  if 'file_url' in selected:
   nonce='URL_FILE_'+secrets.token_hex(12)
   source='https://httpbingo.org/base64/'+urllib.parse.quote(base64.b64encode(('Verification code: '+nonce).encode()).decode(),safe='')
   result=client.responses.create(model='doubao-work-pro',input=[{'role':'user','content':[{'type':'input_text','text':'Read the attached text file. Reply only with its Verification code.'},{'type':'input_file','filename':'verification.txt','file_url':source}]}])
   assert result.output_text==nonce,result.output_text
   record('file_url',exact_fetched_nonce=True,source_not_in_native_prompt=True)
  for protocol in ('chat','responses'):
   if 'tool_media_'+protocol in selected:tool_media(client,protocol)
 report['success']=True
finally:
 name='extended-'+('-'.join(sorted(selected)))+'.json'
 (TASK/'evidence'/name).write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
