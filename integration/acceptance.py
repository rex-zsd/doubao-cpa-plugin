"""Real Chat/Responses media and a tool batch across a native CPA restart."""
import base64,hashlib,json,os,pathlib,secrets,time
from native_probe import NativeCPA,TASK
from sdk_helpers import collect,execute_parallel

FIXTURES=pathlib.Path(__file__).resolve().parent/'fixtures'
expected=json.loads((FIXTURES/'expected.json').read_text())
image='data:image/png;base64,'+base64.b64encode((FIXTURES/'fixture.png').read_bytes()).decode()
pdf='data:application/pdf;base64,'+base64.b64encode((FIXTURES/'fixture.pdf').read_bytes()).decode()
report={'success':False,'native_plugin':True,'cases':[]}

def parsed(text):
 value=text.strip()
 if value.startswith('```'):value=value.split('\n',1)[1].strip().removesuffix('```').strip()
 return json.loads(value)

def media(client,protocol):
 prompt='请实际读取附带的图片和 PDF。只输出 JSON，字段 red_squares 为红色正方形数量，blue_circles 为蓝色圆形数量，code 为 PDF 中 Verification code，sum 为 Amount A 与 Amount B 之和。'
 key='native-media-'+secrets.token_hex(12)
 if protocol=='chat':
  result=client.chat.completions.create(model='doubao-work-pro',messages=[{'role':'user','content':[{'type':'text','text':prompt},{'type':'image_url','image_url':{'url':image}},{'type':'file','file':{'filename':'fixture.pdf','file_data':pdf}}]}],extra_headers={'Idempotency-Key':key})
  text=result.choices[0].message.content
 else:
  result=client.responses.create(model='doubao-work-pro',input=[{'role':'user','content':[{'type':'input_text','text':prompt},{'type':'input_image','image_url':image},{'type':'input_file','filename':'fixture.pdf','file_data':pdf}]}],extra_headers={'Idempotency-Key':key})
  text=result.output_text
 assert parsed(text)==expected,(protocol,text)
 report['cases'].append({'case':protocol+'_media','success':True,'image_and_pdf_exact_match':True,'hidden_answer_not_in_prompt':expected['code'] not in prompt})
 print('NATIVE_MEDIA_VERIFIED',protocol,flush=True)

try:
 cpa=NativeCPA()
 with cpa, cpa.client() as client:
  if not os.environ.get('DOUBAO_SKIP_MEDIA'):
   media(client,'chat')
   media(client,'responses')
  challenge='restart_'+secrets.token_hex(8)
  history=[{'role':'user','content':'请在一个批次中调用 lookup_alpha、lookup_beta、lookup_gamma，每个 challenge 都是 '+challenge+'。它们完全独立，必须一次提交。取得全部真实结果后，只输出 JSON，键为 alpha、beta、gamma，对应各工具的 value。'}]
  key='restart-'+secrets.token_hex(12)
  calls,_,output,count=collect(client,'responses',history,key)
  assert len(calls)==3,calls
  completed,want,parallel=execute_parallel(calls,challenge)
  history+=output
  history +=[{'type':'function_call_output','call_id':c['id'],'output':value} for c,value in completed]
 # The first process unloaded its plugin without cancelling the native pending task.
 with NativeCPA() as resumed,resumed.client() as client:
  initial=history[:1]
  replay,_,_,_=collect(client,'responses',initial,key)
  assert replay==calls,'restart changed pending call identities'
  next_calls,text,_,events=collect(client,'responses',history)
  assert not next_calls and parsed(text)==want,text
  again,replay_text,_,_=collect(client,'responses',history)
  assert not again and replay_text==text,'result replay after restart differed'
  report['cases'].append(dict(case='responses_batch_restart',success=True,**parallel,stable_call_ids_after_restart=True,final_exact_match=True,result_replay_same_answer=True,final_sha256=hashlib.sha256(text.encode()).hexdigest()))
  print('NATIVE_BATCH_RESTART_VERIFIED',flush=True)
 report['success']=True
finally:
 (TASK/('evidence/acceptance-restart.json' if os.environ.get('DOUBAO_SKIP_MEDIA') else 'evidence/acceptance.json')).write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
