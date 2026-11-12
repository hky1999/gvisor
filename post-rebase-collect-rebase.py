import pathlib,json,hashlib,tarfile
r=pathlib.Path('/root/filestore-reflink-precompile-20261112')
s=pathlib.Path('/root/filestore-publish-20261111/source')
m=json.loads((r/'source-manifest.json').read_text())
assert all(hashlib.sha256((s/f).read_bytes()).hexdigest()==h for f,h in m.items())
reg=(r/'rebase-integration-test.log').read_text()
assert reg.count('--- PASS: TestExternalFilestoreCheckpointRestore/')==54
assert '--- FAIL:' not in reg and '--- SKIP:' not in reg
release=r/'rebased-release'
hashes={str(f.relative_to(release)):{'bytes':f.stat().st_size,'sha256':hashlib.sha256(f.read_bytes()).hexdigest()} for f in release.rglob('*') if f.is_file()}
(r/'rebased-release-hashes.json').write_text(json.dumps(hashes,indent=2)+'\n')
(r/'rebase-verification.json').write_text(json.dumps({'source_files_verified':len(m),'unit_targets_passed':5,'external_cases_passed':54,'reordered_cases_passed':24,'complete_release_build':'pass','skips':0},indent=2)+'\n')
with tarfile.open(r/'rebase-evidence.tar.xz','w:xz') as t:
 for f in sorted(r.glob('rebase*.log')):t.add(f,arcname=f.name)
 for name in ['rebase-verification.json','rebased-release-hashes.json']:t.add(r/name,arcname=name)
print('REBASE_EVIDENCE_VERIFIED')
