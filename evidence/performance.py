import os,pathlib,subprocess,json
r=pathlib.Path('/root/filestore-reflink-precompile-20261112')
p=pathlib.Path('/root/filestore-publish-20261111')
benchstat='/root/codex-filestore-20261023/tools/benchstat'
runfiles=p/'source/bazel-bin/runsc/container/boot_benchmark_test_/boot_benchmark_test.runfiles'
env=dict(os.environ,TEST_TMPDIR='/mnt/m0-xfs/filestore-publish-tests',TMPDIR='/mnt/m0-xfs/filestore-publish-tests',TEST_SRCDIR=str(runfiles))
for pair in range(24):
 for enabled in ['disabled','enabled']:
    for v in (['baseline','fixed'] if pair%2==0 else ['fixed','baseline']):
        release=r/(v+'-release')
        e=dict(env,GVISOR_SIDECAR_BINARIES_DIR=str(release/'gvisor-bin'),BENCH_FILESTORE_REFLINK=enabled)
        cmd=[str(r/(v+'-boot.test')),'-test.run=^$','-test.bench=BenchmarkSandboxBoot','-test.benchtime=5x','-test.count=1','--test_platforms=systrap,kvm','--runsc='+str(release/'runsc')]
        with (r/('boot-'+enabled+'-'+v+'.txt')).open('a') as out,(r/('boot-'+enabled+'-'+v+'.stderr')).open('a') as err:
            subprocess.run(cmd,env=e,cwd=runfiles/'_main',stdout=out,stderr=err,check=True)
    print('boot pair',pair+1,flush=True)
for enabled in ['disabled','enabled']:
 with (r/('boot-'+enabled+'-benchstat.txt')).open('w') as out:
    subprocess.run([benchstat,str(r/('boot-'+enabled+'-baseline.txt')),str(r/('boot-'+enabled+'-fixed.txt'))],stdout=out,check=True)
cmd=['python3',str(r/'harness/resume_benchmark.py'),'--baseline',str(r/'baseline-release/runsc'),'--candidate',str(r/'fixed-release/runsc'),'--work-dir','/mnt/m0-xfs/filestore-precompile-perf','--output',str(r/'external-performance'),'--mib','64','--preflush-mode','off','--pairs','24']
with (r/'external-performance.log').open('w') as out:
    subprocess.run(cmd,env=env,stdout=out,stderr=subprocess.STDOUT,check=True)
with (r/'external-benchstat.txt').open('w') as out:
    subprocess.run([benchstat,str(r/'external-performance/baseline-dirty.txt'),str(r/'external-performance/candidate-dirty.txt')],stdout=out,check=True)
print('PERFORMANCE_COMPLETE',flush=True)
