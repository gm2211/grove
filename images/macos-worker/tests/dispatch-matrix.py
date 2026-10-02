#!/usr/bin/env python3
"""Exercise unmodified Grove CLI/API -> real Nomad; no live cluster credentials."""
import json, os, pathlib, secrets, shutil, signal, stat, subprocess, sys, tempfile, time, urllib.request, urllib.parse
MODE=sys.argv[1]
assert MODE in ('lean','bundled','shared')
ASSETS=pathlib.Path(__file__).resolve().parent
GROVE=pathlib.Path(os.environ.get('GROVE_MATRIX_BINARY', str(ASSETS/'grove')))
# Refuse a host/live cluster before creating credentials or changing any jobs.
with urllib.request.urlopen('http://127.0.0.1:4646/v1/nodes',timeout=10) as response:
 nodes=json.load(response)
assert len(nodes)==1 and nodes[0]['Name']=='grove-matrix-'+MODE and nodes[0]['Status']=='ready', 'Expected one ready disposable matrix node'
with urllib.request.urlopen('http://127.0.0.1:4646/v1/jobs',timeout=10) as response:
 assert not json.load(response), 'Refusing a Nomad server with existing jobs'
ROOT=pathlib.Path(tempfile.mkdtemp(prefix='grove-dispatch-matrix-'))
ENV=dict(os.environ,PATH='/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin',GROVE_CONFIG=str(ROOT/'config.json'),XDG_STATE_HOME=str(ROOT/'state'))
PROC=None
TOKEN=''
JOB_IDS=[]
RESULTS=[]

def private_json(path,obj):
 fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as f: json.dump(obj,f)
 assert stat.S_IMODE(path.stat().st_mode)==0o600

def cli(*args,timeout=30):
 argv=[str(ROOT/'grove'),*args]
 assert TOKEN not in '\0'.join(argv)
 result=subprocess.run(argv,env=ENV,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=timeout)
 if TOKEN in result.stdout or TOKEN in result.stderr: raise RuntimeError('Credential appeared in captured output')
 if result.returncode: raise RuntimeError(f'Grove CLI {args[0]} failed ({result.returncode}): {result.stderr}')
 return result.stdout + (result.stderr if args[0] == "logs" else "")

def nomad(path,method='GET'):
 with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:4646'+path,method=method),timeout=10) as r:
  b=r.read()
  return json.loads(b) if b else None

def cleanup():
 global PROC
 failures=[]
 state=ROOT/'state/grove/jobs.json'
 records=json.loads(state.read_text()) if state.exists() else []
 nomad_ids=[r['nomadJobId'] for r in records if r.get('nomadJobId')]
 for job_id in nomad_ids:
  try: nomad('/v1/job/'+urllib.parse.quote(job_id,safe='')+'?purge=true','DELETE')
  except Exception as e: failures.append(type(e).__name__)
 for kind in ('shell','build','agent'):
  try: nomad('/v1/job/grove-'+kind+'-macos?purge=true','DELETE')
  except Exception as e: failures.append(type(e).__name__)
 if PROC is not None:
  try: PROC.terminate()
  except ProcessLookupError: pass
  try: PROC.wait(timeout=10)
  except subprocess.TimeoutExpired: PROC.kill(); PROC.wait()
 shutil.rmtree(ROOT)
 print('CLEANUP '+json.dumps({'mode':MODE,'guestConfigRemoved':not ROOT.exists(),'groveStopped':PROC is None or PROC.poll() is not None,'jobPurgeErrors':failures}),flush=True)
 if failures: raise RuntimeError('Nomad test job cleanup failed: '+str(failures))

def interrupted(signum,frame): raise KeyboardInterrupt()
signal.signal(signal.SIGTERM,interrupted)
signal.signal(signal.SIGINT,interrupted)
try:
 # Verify credential-file permissions/cleanup with a fake value before real use.
 fake=ROOT/'fake-auth.json'; private_json(fake,{'token':'FAKE-MATRIX-SECRET'})
 assert 'FAKE-MATRIX-SECRET' not in str([str(ROOT/'grove'),'serve','--no-fleet-reconcile'])
 fake.unlink(); assert not fake.exists()
 TOKEN=secrets.token_hex(32)
 shutil.copy2(GROVE,ROOT/'grove')
 private_json(ROOT/'fleet.json',{'pools':[{'name':'macos','image':'local-test-'+MODE,'perWorker':1,'jobCPU':500,'jobMemory':2048}]})
 private_json(ROOT/'config.json',{'orchard':{'url':'http://127.0.0.1:16121'},'nomad':{'url':'http://127.0.0.1:4646'},'server':{'listen':'127.0.0.1:16120','url':'http://127.0.0.1:16120','token':TOKEN},'fleet':str(ROOT/'fleet.json')})
 with open(ROOT/'grove-server.log','w') as log:
  PROC=subprocess.Popen([str(ROOT/'grove'),'serve','--no-fleet-reconcile'],env=ENV,stdout=log,stderr=log)
  for attempt in range(60):
   if PROC.poll() is not None: raise RuntimeError('Grove server exited: '+(ROOT/'grove-server.log').read_text().replace(TOKEN,'[REDACTED]'))
   try:
    cli('jobs','ls',timeout=3)
    nomad('/v1/job/grove-shell-macos')
    break
   except Exception: time.sleep(1)
  else: raise RuntimeError('Grove API/job registration not ready')
  print('READY '+json.dumps({'mode':MODE,'groveVersion':cli('--version').strip(),'auth':'ephemeral private guest config','transport':'Grove CLI -> Grove HTTP API -> Nomad dispatch -> raw_exec'}),flush=True)
  for task,expected_code,marker in [('clt',0,'GROVE_CLT_OK'),('xcode',78 if MODE=='lean' else 0,'GROVE_XCODE_REQUIRED' if MODE=='lean' else 'GROVE_XCODE_OK')]:
   script=(ASSETS/(task+'-task.sh')).read_text()
   job_id=cli('dispatch','--pool','macos','--kind','shell','--timeout','6m','--env','GROVE_MATRIX_MODE='+MODE,'--',script).strip()
   JOB_IDS.append(job_id)
   print('SUBMITTED '+json.dumps({'mode':MODE,'task':task,'jobID':job_id}),flush=True)
   deadline=time.monotonic()+420
   while time.monotonic()<deadline:
    job=json.loads(cli('jobs','get',job_id))
    if job['status'] in ('success','failed','lost','canceled'): break
    time.sleep(2)
   else: raise RuntimeError('Job did not terminate: '+job_id)
   logs=cli('logs',job_id)
   alloc=nomad('/v1/allocation/'+job['allocId']) if job.get('allocId') else None
   result={'mode':MODE,'task':task,'jobID':job_id,'status':job['status'],'exitCode':job.get('exitCode'),'allocID':job.get('allocId'),'placement':job.get('placement'),'nomadJobID':alloc.get('JobID') if alloc else None,'nomadClientStatus':alloc.get('ClientStatus') if alloc else None,'nomadDriver':alloc['Job']['TaskGroups'][0]['Tasks'][0]['Driver'] if alloc else None,'logs':logs}
   RESULTS.append(result)
   print('RESULT '+json.dumps(result),flush=True)
   assert job['status']==('success' if expected_code==0 else 'failed'),result
   assert job.get('exitCode')==expected_code,result
   assert job.get('allocId') and result['nomadDriver']=='raw_exec',result
   assert job.get('placement',{}).get('vmId')=='grove-matrix-'+MODE,result
   assert marker in logs,result
  print('MATRIX_PASS '+MODE,flush=True)
finally:
 cleanup()
