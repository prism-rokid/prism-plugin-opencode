#!/usr/bin/env python3
"""PluginBridge smoke against a disposable, real OpenCode V2 server.

Requires a configured model. With the deterministic local provider used during
development, pass --expected PRISM_V2_OK. No global CLI/config is modified.
"""
import argparse, base64, json, os, pathlib, queue, struct, subprocess, threading, time, urllib.request, zlib

ap=argparse.ArgumentParser(description=__doc__)
ap.add_argument('--server-url',required=True)
ap.add_argument('--cwd',required=True)
ap.add_argument('--model',required=True)
ap.add_argument('--expected',default='PRISM_V2_OK')
ap.add_argument('--vision-check',action='store_true',help='Require the real model to identify the generated image color; do not use with the fixed-response mock')
ap.add_argument('--adapter',default=str(pathlib.Path(__file__).resolve().parents[1]/'bin/prism-plugin-opencode'))
args=ap.parse_args()
password=os.environ.get('OPENCODE_PASSWORD',os.environ.get('OPENCODE_SERVER_PASSWORD',''))
auth='Basic '+base64.b64encode(('opencode:'+password).encode()).decode()
def http(path):
 with urllib.request.urlopen(urllib.request.Request(args.server_url+path,headers={'Authorization':auth}),timeout=20) as r:return json.load(r)
assert http('/api/info')['version'].startswith('2.')
p=subprocess.Popen([args.adapter,'--server-url',args.server_url],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env=dict(os.environ,PRISM_PLUGIN_PROTOCOL_VERSION='4'))
frames=[];q=queue.Queue();seq=0
def read():
 for line in p.stdout:
  f=json.loads(line);frames.append(f);q.put(f)
threading.Thread(target=read,daemon=True).start()
def rpc(method,params=None,timeout=30):
 global seq
 seq+=1;ident=str(seq);p.stdin.write(json.dumps({'id':ident,'method':'adapter.'+method,'params':params})+'\n');p.stdin.flush()
 deadline=time.monotonic()+timeout
 while time.monotonic()<deadline:
  f=q.get(timeout=max(.01,deadline-time.monotonic()))
  if f.get('id')==ident:
   assert f.get('ok'),(method,f);print('PASS',method,flush=True);return f.get('payload')
 raise TimeoutError(method)
def wait(predicate,label,timeout=20):
 deadline=time.monotonic()+timeout
 while time.monotonic()<deadline:
  if predicate():return
  time.sleep(.05)
 raise TimeoutError(label)
def history(session,stream):
 rpc('readHistoryStream',{'session':session,'request':{'stream_id':stream,'limit':20}})
 wait(lambda:any(f.get('event')=='history.stream' and f['payload'].get('stream_id')==stream and f['payload'].get('type')=='end' for f in frames),stream)
 return [m for f in frames if f.get('event')=='history.stream' and f['payload'].get('stream_id')==stream and f['payload'].get('turn') for m in f['payload']['turn']['messages']]
def image_answer_matches(text):
 return ('red' in text.lower() or '红' in text) if args.vision_check else args.expected in text
try:
 assert rpc('probe')['available'];assert rpc('discover')['verified']
 draft=rpc('openDraft',{'draft_id':'v2-smoke-'+str(time.time_ns()),'cwd':args.cwd})
 rpc('controlDraft',{'draft_id':draft['draft_id'],'action':'model.switch','target':{'option_id':args.model}})
 started=rpc('startDraftWithMessage',{'draft_id':draft['draft_id'],'cwd':args.cwd,'message':{'prism_message_id':'v2-first','text':'Reply exactly '+args.expected+'. Do not use tools.'}})
 session=started['session'];sid=session['native_session_id'];assert session['cwd']==args.cwd
 assert started['receipt']['accepted'] and started['visibility']['visible']
 # Intentionally wait AFTER native completion to verify missed-event recovery.
 wait(lambda:bool(http('/api/session/'+sid)['data']['time'].get('idle')),'native idle')
 end=rpc('waitForRun',{'session':session,'run_id':'v2-first'})
 assert end['type']=='run.completed' and args.expected in end['summary'],end
 assert any(x['native_session_id']==sid for x in rpc('listSessions'))
 assert rpc('controlSession',{'session':session,'action':'conversation.select'})['ok']
 detail=rpc('readDetail',{'session':session});assert detail['current_model']['value']==args.model and detail['model_options']
 rpc('controlSession',{'session':session,'action':'model.switch','target':args.model})
 rpc('subscribe',{'subscription_id':'v2-live','session':session})
 rpc('readHistoryStream',{'session':session,'request':{'stream_id':'v2-live','live':True,'limit':20}})
 time.sleep(.25)
 png=pathlib.Path(args.cwd)/'prism-v2-smoke.png'
 def chunk(kind,data):return struct.pack('>I',len(data))+kind+data+struct.pack('>I',zlib.crc32(kind+data)&0xffffffff)
 png.write_bytes(b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',16,16,8,2,0,0,0))+chunk(b'IDAT',zlib.compress((b'\x00'+b'\xff\x00\x00'*16)*16))+chunk(b'IEND',b''))
 image_prompt='What is the dominant color of the attached image? Answer briefly based on the image only. Do not use tools.' if args.vision_check else 'Reply exactly '+args.expected+'.'
 assert rpc('send',{'session':session,'message':{'prism_message_id':'v2-image','text':image_prompt,'attachments':[{'name':png.name,'mime_type':'image/png','local_path':str(png)}]}})['accepted']
 end=rpc('waitForRun',{'session':session,'run_id':'v2-image'});assert end['type']=='run.completed',end
 if args.vision_check:
  assert image_answer_matches(end['summary']),('Image was admitted but real visual recognition failed',end)
  print('PASS real visual recognition: red image',flush=True)
 messages=history(session,'v2-final')
 assert any(m['role']=='assistant' and args.expected in m.get('content','') for m in messages),messages
 assert any(m.get('metadata',{}).get('attachments') for m in messages if m['role']=='user'),messages
 wait(lambda:any(f.get('event')=='plugin.event' and f['payload']['event']['type']=='message.part.updated' and image_answer_matches(f['payload']['event'].get('payload',{}).get('text','')) for f in frames),'streamed text')
 assert any(f.get('event')=='history.stream' and f['payload'].get('source')=='live' and f['payload'].get('turn') and any(image_answer_matches(m.get('content','')) for m in f['payload']['turn']['messages']) for f in frames),'missing live history'
 # Interrupt the next execution before its provider responds.
 offset=len(frames)
 rpc('send',{'session':session,'message':{'prism_message_id':'v2-interrupt','text':'Reply after a long delay.'}})
 rpc('interrupt',{'session':session})
 end=rpc('waitForRun',{'session':session,'run_id':'v2-interrupt'});assert end['type']=='run.failed',end
 rpc('cancelHistoryStream',{'stream_id':'v2-live'});rpc('unsubscribe',{'subscription_id':'v2-live'});rpc('close')
 print('V2 REAL SERVER SMOKE PASS',flush=True)
finally:
 p.stdin.close()
 try:p.wait(timeout=5)
 except subprocess.TimeoutExpired:p.kill();p.wait()
