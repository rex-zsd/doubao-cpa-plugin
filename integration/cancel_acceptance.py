"""Cancel only the run created by this test; never repeat its stop mutation."""
import json,pathlib,secrets,subprocess,time
from native_probe import NativeCPA,TASK

report={'success':False,'native_plugin':True}
try:
 with NativeCPA() as cpa,cpa.client() as client:
  challenge=secrets.token_hex(8)
  tools=[{'type':'function','function':{'name':'await_value','description':'Get a value from the caller.','parameters':{'type':'object','properties':{'challenge':{'type':'string'}},'required':['challenge']}}}]
  history=[{'role':'user','content':'Call await_value with challenge '+challenge+' and wait for its real result.'}]
  first=client.chat.completions.create(model='doubao-work-pro',messages=history,tools=tools)
  message=first.choices[0].message;assert len(message.tool_calls)==1
  call=message.tool_calls[0]
  paths=[]
  for path in (TASK/'private').glob('gateway-state-*.json'):
   state=json.loads(path.read_text())
   if any(p['id']==call.id for p in state['pending']):paths.append(path)
  assert len(paths)==1
  path=paths[0];state=json.loads(path.read_text())
  command=[str(TASK/'build/doubao-helper'),'-dir',str(TASK/'private')]
  cancelled=subprocess.run(command+['-action','task-cancel','-task-id',state['id']],text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=40)
  assert cancelled.returncode==0,'one-time targeted cancellation was not accepted'
  receipt=TASK/'private'/('gateway-receipt-'+state['id']+'.json')
  confirmed=False
  for _ in range(30):
   check=subprocess.run(command+['-action','tool-status','-receipt',str(receipt),'-out',str(TASK/'private/cancel-status')],text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=30)
   evidence=json.loads((TASK/'private/cancel-status/latest.json').read_text())
   confirmed=evidence.get('cancellation_confirmed',False)
   state=json.loads(path.read_text())
   if confirmed and state['done']:break
   time.sleep(.2)
  assert confirmed and state['done'],'service-side cancellation not confirmed'
  history +=[message.model_dump(exclude_none=True),{'role':'tool','tool_call_id':call.id,'content':'must not be delivered after cancellation'}]
  rejected=False
  try:client.chat.completions.create(model='doubao-work-pro',messages=history,tools=tools)
  except Exception as exc:rejected=getattr(exc,'status_code',None)==409
  assert rejected,'cancelled task accepted another result'
  report.update(success=True,targeted_stop_accepted=True,cancellation_confirmed=True,late_result_rejected=True,primary_generation_posts=state['generation_posts'])
finally:
 (TASK/'evidence/cancellation.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
