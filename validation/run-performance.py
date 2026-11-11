#!/usr/bin/env python3
"""Alternate current-upstream and narrowed-PR runs on the existing XFS test mount."""
import json, os, pathlib, subprocess, time
ROOT = pathlib.Path('/root/filestore-publish-20261111')
OUT = ROOT/'final-performance'
OUT.mkdir(exist_ok=True)
WORK = pathlib.Path('/mnt/m0-xfs')
BENCHSTAT = '/root/codex-filestore-20261023/tools/benchstat'
env = dict(os.environ, TMPDIR=str(WORK/'filestore-publish-tests'))
# Existing, broad ordinary-save benchmarks. Cover all their subcases.
for pair in range(0 if (OUT/'broad-benchstat.txt').exists() else 24):
    for variant in (['upstream','candidate'] if pair % 2 == 0 else ['candidate','upstream']):
        tree = 'upstream' if variant == 'upstream' else 'source'
        binary = ROOT/tree/'bazel-bin/pkg/sentry/pgalloc/pgalloc_test_/pgalloc_test'
        with (OUT/f'broad-{variant}.txt').open('a') as output:
            subprocess.run([str(binary), '-test.run=^$', '-test.bench=BenchmarkSaveTo',
                            '-test.benchtime=1x', '-test.count=1'], env=env, stdout=output, stderr=output, check=True)
    print(json.dumps({'broad_pair_complete':pair+1}),flush=True)
with (OUT/'broad-benchstat.txt').open('w') as output:
    subprocess.run([BENCHSTAT,str(OUT/'broad-upstream.txt'),str(OUT/'broad-candidate.txt')],stdout=output,check=True)
for name, extra in [('fresh-1g',['--mib','1024']),
                    ('sparse-4g-64m',['--mib','4096','--sparse-mib','64']),
                    ('dense-4g-64m',['--mib','4096','--rewrite-mib','64'])]:
    command=['python3',str(ROOT/'harness/resume_benchmark.py'),
             '--baseline',str(ROOT/'upstream-release/runsc'),
             '--candidate',str(ROOT/'final-measured-release/runsc'),
             '--work-dir',str(WORK/('filestore-publish-'+name)),
             '--output',str(OUT/name),'--preflush-mode','off','--pairs','12']+extra
    print(json.dumps({'start':name,'command':command,'time':time.time()}),flush=True)
    with (OUT/(name+'.log')).open('w') as output:
        subprocess.run(command,stdout=output,stderr=output,check=True)
    with (OUT/name/'benchstat.txt').open('w') as output:
        subprocess.run([BENCHSTAT,str(OUT/name/'baseline-dirty.txt'),str(OUT/name/'candidate-dirty.txt')],stdout=output,check=True)
    print(json.dumps({'complete':name,'time':time.time()}),flush=True)
