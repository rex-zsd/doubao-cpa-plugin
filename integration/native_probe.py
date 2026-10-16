"""Live native-plugin harness. Private state is never included in public evidence."""
import argparse,json,os,pathlib,secrets,shutil,socket,subprocess,time,urllib.request
import openai

ROOT=pathlib.Path(__file__).resolve().parents[1]
if not os.environ.get('DOUBAO_TEST_DIR'):raise RuntimeError('Set DOUBAO_TEST_DIR to a private acceptance directory containing build/ and private/session.json')
TASK=pathlib.Path(os.environ['DOUBAO_TEST_DIR']).resolve()
class NativeCPA:
 def __init__(self):
  with socket.socket() as s:s.bind(('127.0.0.1',0));self.port=s.getsockname()[1]
  self.base=f'http://127.0.0.1:{self.port}/v1'
  self.state=TASK/'private';self.work=self.state/'host';self.work.mkdir(exist_ok=True)
  auth=self.work/'auths';auth.mkdir(exist_ok=True);(auth/'doubao-work.json').write_text(json.dumps({'type':'doubao-work','version':1}))
  plugins=self.work/'plugins';plugins.mkdir(exist_ok=True)
  dst=plugins/'doubao-work.so'
  if dst.is_symlink():dst.unlink()
  shutil.copyfile(TASK/'build/doubao-work.so',dst)
  self.config=self.work/'config.yaml'
  self.config.write_text(f'''config-version: 8
server:
  host: 127.0.0.1
  port: {self.port}
management:
  disable-control-panel: true
access:
  api-keys: [native-local-test]
oauth:
  auth-dir: {auth}
routing:
  strategy: fill-first
  retry:
    request-retry: 0
    max-retry-credentials: 1
requests:
  streaming:
    bootstrap-retries: 0
plugins:
  enabled: true
  dir: {plugins}
  configs:
    doubao-work:
      enabled: true
      priority: 1
      state_dir: {self.state}
      helper_path: {TASK/'build/doubao-helper'}
''')
 def __enter__(self):
  self.log=(self.work/'cpa.log').open('w')
  self.proc=subprocess.Popen([str(TASK/'build/cli-proxy-api'),'--local-model','--config',str(self.config)],cwd=self.work,stdout=self.log,stderr=subprocess.STDOUT)
  for _ in range(300):
   if self.proc.poll() is not None:raise RuntimeError('native CPA stopped; inspect private host log')
   try:
    request=urllib.request.Request(self.base+'/models',headers={'Authorization':'Bearer native-local-test'})
    with urllib.request.urlopen(request,timeout=2) as r:
     models=json.load(r)
     if any(m['id']=='doubao-work-pro' for m in models['data']):return self
   except(OSError,TimeoutError):pass
   time.sleep(.1)
  self.__exit__();raise RuntimeError('native model registration timeout')
 def __exit__(self,*_):
  if self.proc.poll() is None:
   self.proc.terminate()
   try:self.proc.wait(50)
   except subprocess.TimeoutExpired:self.proc.kill();self.proc.wait()
  self.log.close()
 def client(self):return openai.OpenAI(base_url=self.base,api_key='native-local-test',max_retries=0,timeout=240)

if __name__=='__main__':
 report={'native_plugin':True,'reference_library_used':False,'success':False}
 try:
  with NativeCPA() as cpa, cpa.client() as client:
   marker='NATIVE_PLUGIN_'+secrets.token_hex(10)
   result=client.chat.completions.create(model='doubao-work-pro',messages=[{'role':'user','content':'请只原样回复：'+marker}],extra_headers={'Idempotency-Key':'native-'+secrets.token_hex(12)})
   assert result.choices[0].message.content==marker,result
   report.update(success=True,exact_match=True)
 finally:
  (TASK/'evidence/native-smoke.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
