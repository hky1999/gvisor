#!/usr/bin/env python3
"""Alternate current-upstream and narrowed-PR runs on the existing XFS test mount."""
import json, os, pathlib, subprocess, time
ROOT = pathlib.Path('/root/filestore-publish-20261111')
OUT = ROOT/'broad-stable'
OUT.mkdir(exist_ok=False)
WORK = pathlib.Path('/mnt/m0-xfs')
BENCHSTAT = '/root/codex-filestore-20261023/tools/benchstat'
env = dict(os.environ, TMPDIR=str(WORK/'filestore-publish-tests'))
# Existing, broad ordinary-save benchmarks. Cover all their subcases.
for pair in range(24):
    for variant in (['upstream','candidate'] if pair % 2 == 0 else ['candidate','upstream']):
        tree = 'upstream' if variant == 'upstream' else 'source'
        binary = ROOT/tree/'bazel-bin/pkg/sentry/pgalloc/pgalloc_test_/pgalloc_test'
        with (OUT/f'broad-{variant}.txt').open('a') as output, (OUT/f'broad-{variant}.stderr.log').open('a') as errors:
            subprocess.run([str(binary), '-test.run=^$', '-test.bench=BenchmarkSaveTo',
                            '-test.benchtime=5x', '-test.count=1'], env=env, stdout=output, stderr=errors, check=True)
    print(json.dumps({'broad_pair_complete':pair+1}),flush=True)
with (OUT/'broad-benchstat.txt').open('w') as output:
    subprocess.run([BENCHSTAT,str(OUT/'broad-upstream.txt'),str(OUT/'broad-candidate.txt')],stdout=output,check=True)
