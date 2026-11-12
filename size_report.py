import pathlib,json,hashlib,re,platform,subprocess
r=pathlib.Path('/root/filestore-reflink-precompile-20261112')
prior=pathlib.Path('/root/filestore-mount-identity-20261111/fixed-release')
result={'host':platform.platform(),'cpu':subprocess.check_output(['lscpu'],text=True),'variants':{}}
for v in ['baseline','fixed']:
    release=r/(v+'-release')
    files={str(p.relative_to(release)):{'bytes':p.stat().st_size,'sha256':hashlib.sha256(p.read_bytes()).hexdigest()} for p in sorted(release.rglob('*')) if p.is_file()}
    source=(r/(v+'-filter_precompiled.go')).read_text()
    result['variants'][v]={'files':files,'release_file_bytes':sum(x['bytes'] for x in files.values()),'generated_go_bytes':len(source.encode()),'program_count':len(re.findall(r'^\s*case ',source,re.M))}
result['baseline_matches_previously_published_cd7_release']=all((prior/p).is_file() and hashlib.sha256((prior/p).read_bytes()).hexdigest()==d['sha256'] for p,d in result['variants']['baseline']['files'].items())
(r/'sizes.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps({v:{k:d[k] for k in ['release_file_bytes','generated_go_bytes','program_count']} for v,d in result['variants'].items()},indent=2))
print('baseline matches cd7 release:',result['baseline_matches_previously_published_cd7_release'])
