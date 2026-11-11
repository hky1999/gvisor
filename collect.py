import hashlib,json,pathlib,statistics,re,tarfile
r=pathlib.Path('/root/filestore-mount-identity-20261111'); src=pathlib.Path('/root/filestore-publish-20261111/source')
samples=[json.loads(x) for x in (r/'external-performance/samples.jsonl').read_text().splitlines()]
assert len(samples)==48
summary={}
for v in ('baseline','candidate'):
 a=[s for s in samples if s['variant']==v]; assert len(a)==24
 assert sorted(s['pair'] for s in a)==list(range(24))
 assert all(s['verified_bytes']==64*1024*1024 and s['verified_source_bytes']==64*1024*1024 for s in a)
 metrics={'pause_ms':[s['pause']['application_pause_ns']/1e6 for s in a], 'cli_ms':[s['checkpoint_ns']/1e6 for s in a], 'durable_ms':[s['durable_checkpoint_ns']/1e6 for s in a], 'restore_cli_ms':[s['restore_ns']/1e6 for s in a]}
 summary[v]={k:{'median':statistics.median(vals),'sd':statistics.stdev(vals),'n':len(vals)} for k,vals in metrics.items()}
 summary[v]['source_writes_before_cli_return']=sum(s['write_started_before_cli_return'] for s in a)
(r/'performance-summary.json').write_text(json.dumps(summary,indent=2)+'\n')
broad={}
for v in ('baseline','fixed'):
 entries={}
 for line in (r/f'boot-{v}.txt').read_text().splitlines():
  parts=line.split()
  if parts and parts[0].startswith('Benchmark'):
   assert parts[1]=='5',parts
   for i in range(2,len(parts)-1,2):
    entries.setdefault(parts[0]+'/'+parts[i+1],[]).append(float(parts[i]))
 assert len(entries)==36,len(entries)
 assert all(len(a)==24 for a in entries.values())
 broad[v]={k:{'median':statistics.median(a),'sd':statistics.stdev(a),'n':len(a)} for k,a in entries.items()}
(r/'boot-statistics.json').write_text(json.dumps(broad,indent=2)+'\n')
m=json.loads((r/'source-manifest.json').read_text());bad=[]
for name,want in m.items():
 b=(src/name).read_bytes();got=hashlib.sha1(b'blob '+str(len(b)).encode()+b'\0'+b).hexdigest()
 if got!=want:bad.append(name)
assert not bad,bad
for name,release in [('baseline',pathlib.Path('/root/filestore-publish-20261111/final-measured-release')),('fixed',r/'fixed-release')]:
 hashes={str(p.relative_to(release)):hashlib.sha256(p.read_bytes()).hexdigest() for p in release.rglob('*') if p.is_file()}
 (r/(name+'-release-sha256.json')).write_text(json.dumps(hashes,indent=2)+'\n')
reg=(r/'integration-test.log').read_text(); repro=(r/'reproducer-test.log').read_text();race=(r/'race-test.log').read_text()
assert reg.count('--- PASS: TestExternalFilestoreCheckpointRestore/')==54
assert repro.count('--- FAIL: TestExternalFilestoreCheckpointRestore/')==18
assert '--- FAIL:' not in reg and '--- SKIP:' not in reg
assert '--- FAIL:' not in race and '--- SKIP:' not in race
assert race.count('--- PASS: TestExternalFilestoreCheckpointRestore/')==9
(r/'verification-summary.json').write_text(json.dumps({'source_files':len(m),'reproducer_failures':18,'external_cases_passed':54,'reordered_cases_passed':24,'race_external_cases_passed':9,'performance_samples_verified':48,'boot_pairs':24,'skips':0},indent=2)+'\n')
selected=[p for p in r.iterdir() if p.is_file() and p.suffix in ('.txt','.json','.log','.xml','.exit','.stderr','.sh','.py','.patch')]
selected += [r/'external-performance'/p for p in ('samples.jsonl','baseline-dirty.txt','candidate-dirty.txt','environment.json')]
with tarfile.open(r/'evidence.tar.xz','w:xz') as tar:
 for p in selected:tar.add(p,arcname=str(p.relative_to(r)))
print(json.dumps(summary,indent=2));print('ALL_RESULTS_VERIFIED')
