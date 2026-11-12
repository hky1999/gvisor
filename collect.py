import json,pathlib,statistics,re,tarfile
r=pathlib.Path('/root/filestore-reflink-precompile-20261112')
def stats(a):
 return {'median':statistics.median(a),'mean':statistics.mean(a),'sd':statistics.stdev(a),'n':len(a)}
samples=[json.loads(x) for x in (r/'external-performance/samples.jsonl').read_text().splitlines()]
assert len(samples)==48
summary={}
traces={}
for v in ('baseline','candidate'):
 a=[s for s in samples if s['variant']==v]
 assert len(a)==24 and sorted(s['pair'] for s in a)==list(range(24))
 assert all(s['verified_bytes']==64*1024*1024 and s['verified_source_bytes']==64*1024*1024 for s in a)
 metrics={'pause_ms':[s['pause']['application_pause_ns']/1e6 for s in a], 'cli_ms':[s['checkpoint_ns']/1e6 for s in a], 'durable_ms':[s['durable_checkpoint_ns']/1e6 for s in a], 'restore_cli_ms':[s['restore_ns']/1e6 for s in a]}
 summary[v]={k:stats(vals) for k,vals in metrics.items()}
 summary[v]['source_writes_before_cli_return']=sum(s['write_started_before_cli_return'] for s in a)
 traces[v]={'fallback':[],'loaded':[]}
 for directory in (r/'external-performance').glob('*-'+v+'-dirty'):
  for f in directory.rglob('*'):
   if f.is_file():
    for line in f.read_text(errors='replace').splitlines():
     if 'FilestoreReflink=true' not in line:continue
     for key,marker in [('fallback','building seccomp program from scratch'),('loaded','Loaded precompiled seccomp instructions')]:
      if marker in line:traces[v][key].append({'path':str(f.relative_to(r)),'line':line})
assert len(traces['baseline']['fallback'])>=24
assert len(traces['candidate']['loaded'])>=24
assert not traces['candidate']['fallback']
(r/'cache-path-evidence.json').write_text(json.dumps(traces,indent=2)+'\n')
(r/'performance-summary.json').write_text(json.dumps(summary,indent=2)+'\n')
broad={}
for enabled in ('disabled','enabled'):
 broad[enabled]={}
 for v in ('baseline','fixed'):
  entries={}
  for line in (r/f'boot-{enabled}-{v}.txt').read_text().splitlines():
   parts=line.split()
   if parts and parts[0].startswith('Benchmark'):
    assert parts[1]=='5',parts
    for i in range(2,len(parts)-1,2):entries.setdefault(parts[0]+'/'+parts[i+1],[]).append(float(parts[i]))
  assert len(entries)==36,len(entries)
  assert all(len(a)==24 for a in entries.values())
  broad[enabled][v]={k:stats(a) for k,a in entries.items()}
(r/'boot-statistics.json').write_text(json.dumps(broad,indent=2)+'\n')
reg=(r/'integration-test.log').read_text()
assert reg.count('--- PASS: TestExternalFilestoreCheckpointRestore/')==54
assert '--- FAIL:' not in reg and '--- SKIP:' not in reg
for name in ['config-test.log','filter-test.log']:
 txt=(r/name).read_text()
 assert '--- FAIL:' not in txt and '--- SKIP:' not in txt
assert 'all 1152 configurations include both' in (r/'config-test.log').read_text()
(r/'verification-summary.json').write_text(json.dumps({'external_cases_passed':54,'reordered_cases_passed':24,'all_integration_test_pass_lines':reg.count('--- PASS:'),'generated_filter_cases':8,'generated_programs':1152,'performance_samples_verified':48,'boot_pairs_per_flag':24,'cache_path_counts':{v:{k:len(a) for k,a in d.items()} for v,d in traces.items()},'skips':0},indent=2)+'\n')
selected=[p for p in r.iterdir() if p.is_file() and p.suffix in ('.txt','.json','.log','.xml','.exit','.stderr','.sh','.py','.patch')]
selected += [r/'external-performance'/p for p in ('samples.jsonl','baseline-dirty.txt','candidate-dirty.txt','environment.json')]
with tarfile.open(r/'evidence.tar.xz','w:xz') as tar:
 for p in selected:tar.add(p,arcname=str(p.relative_to(r)))
print(json.dumps(summary,indent=2));print('ALL_RESULTS_VERIFIED')
