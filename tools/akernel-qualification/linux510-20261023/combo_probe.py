"""PR #10 anonymous tmpfs filestore + PR #11 parent-mountns regression."""
import argparse
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import tarfile
import tempfile
import time
import traceback

def mapped(path):
    result = []
    start = 0
    with path.open('rb', buffering=0) as file:
        while True:
            buffer = bytearray(32 + 56 * 1024)
            struct.pack_into('=QQIIII', buffer, 0, start, (1 << 64)-1-start, 1, 0, 1024, 0)
            fcntl.ioctl(file.fileno(), 0xC020660B, buffer, True)
            count = struct.unpack_from('=I', buffer, 20)[0]
            if not count:
                break
            for index in range(count):
                logical, physical, length, _, _, flags, _, _, _ = struct.unpack_from('=QQQQQIIII', buffer, 32+56*index)
                assert not flags & 6, (path, flags)
                result.append({'logical':logical,'physical':physical,'length':length,'flags':flags})
            if result[-1]['flags'] & 1:
                break
            start = result[-1]['logical'] + result[-1]['length']
    return result

def extent_account(paths):
    rows = [{'file':str(p),'size':p.stat().st_size,'device':p.stat().st_dev,'extents':mapped(p)} for p in paths]
    intervals = {}
    for row in rows:
        intervals.setdefault(row['device'],[]).extend((e['physical'],e['physical']+e['length']) for e in row['extents'])
    unique = 0
    for values in intervals.values():
        end = -1
        for begin, stop in sorted(values):
            unique += max(0, stop-max(begin,end))
            end = max(end,stop)
    allocated = sum(e['length'] for r in rows for e in r['extents'])
    return {'files':rows,'mapped_bytes_sum':allocated,'unique_physical_data_bytes':unique,
            'saved_bytes':allocated-unique,'saving_ratio':(allocated-unique)/allocated if allocated else 0,
            'shared_flag_bytes_sum':sum(e['length'] for r in rows for e in r['extents'] if e['flags'] & 0x2000)}

def sparse_digest(path):
    # Hash every allocated data byte, with logical offsets; holes stay holes.
    h = hashlib.sha256()
    size = path.stat().st_size
    h.update(struct.pack('=Q',size))
    with path.open('rb',buffering=0) as file:
        offset = 0
        while offset < size:
            try:
                begin = os.lseek(file.fileno(),offset,os.SEEK_DATA)
            except OSError as error:
                if error.errno == errno.ENXIO:
                    break
                raise
            end = min(os.lseek(file.fileno(),begin,os.SEEK_HOLE),size)
            h.update(struct.pack('=QQ',begin,end-begin))
            file.seek(begin)
            remaining = end-begin
            while remaining:
                chunk = file.read(min(1048576,remaining))
                assert chunk
                h.update(chunk)
                remaining -= len(chunk)
            offset = end
    return h.hexdigest()

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--assets',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--platform',choices=['systrap','kvm'],required=True)
    parser.add_argument('--cgroup',choices=['v1','v2'],required=True)
    parser.add_argument('--runsc',default='/usr/local/bin/runsc')
    parser.add_argument('--work-root',type=Path,default=Path('/home/akernel'))
    args = parser.parse_args()
    mode = 'v2' if Path('/sys/fs/cgroup/cgroup.controllers').exists() else 'v1'
    assert mode == args.cgroup,(mode,args.cgroup)
    assert os.uname().release == '5.10.134-19.8.al8.x86_64',os.uname().release
    args.work_root.mkdir(parents=True,exist_ok=True)
    base = Path(tempfile.mkdtemp(prefix='pr10-pr11-',dir=args.work_root))
    live = base/'live';live.mkdir()
    bundle = base/'bundle';(bundle/'rootfs').mkdir(parents=True)
    source = base/'tmpfs-source';source.mkdir()
    (base/'checkpoints').mkdir()
    report = {'kernel':os.uname().release,'cgroup_mode':mode,'platform':args.platform,
              'runsc_sha256':hashlib.sha256(Path(args.runsc).read_bytes()).hexdigest(),
              'cycles':[],'status':'running'}
    created = []
    call_id = 0
    def invoke(values,check=True,cwd=None):
        nonlocal call_id
        call_id += 1
        # Detached gofer/sentry processes inherit stdio. Pipes would keep
        # communicate() waiting for their lifetime after runsc create exits.
        with (base/f'stdout-{call_id}.log').open('w+b') as out, (base/f'stderr-{call_id}.log').open('w+b') as err:
            p = subprocess.run([args.runsc,*values],stdout=out,stderr=err,timeout=180,cwd=cwd)
            out.seek(0);err.seek(0)
            p.stdout = out.read().decode(errors='replace')
            p.stderr = err.read().decode(errors='replace')
        (base/f'command-{call_id}.json').write_text(json.dumps({'argv':values,'code':p.returncode,'stdout':p.stdout[-3000:],'stderr':p.stderr[-5000:]},indent=2))
        if check and p.returncode:
            raise RuntimeError(f'runsc failed ({p.returncode}): {values}: {p.stderr[-3500:]}')
        return p
    def flags(parent=True,adopt=None):
        values=['--root',str(base/'state'),'--network=none','--platform='+args.platform,'--ignore-cgroups',
                '--overlay2=root:dir='+str(live)+',size=256M']
        if parent:
            values.append('--filestore-parent-mountns=true')
        if adopt:
            values += ['--filestore-adopt-dir='+str(adopt),'--filestore-clone-on-adopt=true']
        return values
    def guest(cid,values):
        return invoke(flags()+['exec',cid,*values]).stdout.strip()
    def state(cid):
        for _ in range(80):
            try:
                return json.loads(guest(cid,['/bin/cat','/probe-state.json']))
            except (ValueError,RuntimeError):
                time.sleep(.1)
        raise RuntimeError('state unavailable')
    def verify(cid,reference,digest):
        current=state(cid)
        for key in ['marker','pid','fd_text','fd_offset','ram_sha256','ram_bytes']:
            assert current[key] == reference[key],(key,current,reference)
        assert current['counter'] >= reference['counter']
        assert guest(cid,['/bin/sha256sum','/data/payload']).split()[0] == digest
        assert guest(cid,['/bin/stat','-f','-c','%T','/data']) == 'tmpfs'
        return current
    def capture(cid,path,check=True):
        path.mkdir()
        return invoke(flags()+['checkpoint','--leave-running','--compression=none','--skip-filestore-pages',
                      '--filestore-snapshot-dir='+str(path/'filestores'),'--image-path',str(path),cid],check=check)
    def delete(cid):
        invoke(flags()+['delete','--force',cid]);created.remove(cid)
    def restore(cid,path):
        # Adoption already creates private clones in the launcher namespace.
        invoke(flags(parent=False,adopt=path/'filestores')+['restore','--bundle',str(bundle),'--image-path',str(path),'--detach',cid])
        created.append(cid)
    def artifact_hashes():
        return {str(p.relative_to(base)):sparse_digest(p) for p in (base/'checkpoints').glob('*/filestores/filestore-*')}
    try:
        with tarfile.open(args.assets/'rootfs.tar.gz','r:gz') as archive:
            archive.extractall(bundle/'rootfs',filter='fully_trusted')
        os.chown(bundle/'rootfs',0,0)
        shutil.copy2(args.assets/'tmpfs-worker',bundle/'rootfs/probe')
        invoke(['spec'],cwd=str(bundle))
        specpath=bundle/'config.json';spec=json.loads(specpath.read_text())
        spec['process']['args']=['/probe'];spec['process']['cwd']='/';spec['process']['terminal']=False
        spec['process']['env']=['PATH=/bin:/usr/bin'];spec['process']['user']={'uid':0,'gid':0}
        spec['root']={'path':'rootfs','readonly':False}
        spec['mounts'].append({'source':str(source),'destination':'/data','type':'bind','options':['rbind','rw']})
        spec['annotations']={'dev.gvisor.spec.mount.data.source':str(source),'dev.gvisor.spec.mount.data.type':'tmpfs',
                             'dev.gvisor.spec.mount.data.share':'container','dev.gvisor.spec.mount.data.filestore':'anonymous'}
        specpath.write_text(json.dumps(spec))
        report['runsc_version']=invoke(['--version']).stdout.strip()
        invoke(flags(parent=False)+['create','--bundle',str(bundle),'negative']);created.append('negative')
        invoke(flags(parent=False)+['start','negative'])
        state('negative')
        negative=capture('negative',base/'negative',check=False)
        assert negative.returncode and ('cross-device' in negative.stderr.lower() or 'exdev' in negative.stderr.lower()),negative.stderr
        report['without_parent_mountns_expected_exdev']=True
        delete('negative')
        invoke(flags()+['create','--bundle',str(bundle),'source']);created.append('source')
        invoke(flags()+['start','source'])
        initial=state('source')
        assert initial['fd_offset']==6 and initial['fd_text']=='tmpfs-preserved-open-fd'
        assert guest('source',['/bin/stat','-f','-c','%T','/data'])=='tmpfs'
        assert not list(source.iterdir()),'tmpfs data leaked into host bind directory'
        for number in range(1,6):
            if number>1:
                guest('source',['/bin/dd','if=/dev/urandom','of=/data/payload','bs=1048576','count=1',f'seek={number-2}','conv=notrunc'])
            before=state('source')
            digest=guest('source',['/bin/sha256sum','/data/payload']).split()[0]
            path=base/'checkpoints'/str(number)
            started=time.monotonic();capture('source',path)
            sidecar=json.loads((path/'filestores/filestores.json').read_text())
            entries=sidecar['filestores']
            assert len(entries)==2,entries
            assert {entry['resource_path'] for entry in entries} == {'/','/data'},entries
            report['cycles'].append({'number':number,'checkpoint_seconds':time.monotonic()-started,
                                      'before':before,'payload_sha256':digest,'manifest':sidecar})
            args.output.write_text(json.dumps(report,indent=2)+'\n')
        original=artifact_hashes()
        report['filestore_extent_accounting']=extent_account(list((base/'checkpoints').glob('*/filestores/filestore-*')))
        assert report['filestore_extent_accounting']['saved_bytes']>0
        delete('source')
        for row in report['cycles']:
            path=base/'checkpoints'/str(row['number'])
            started=time.monotonic();restore('restored',path)
            row['restored_state']=verify('restored',row['before'],row['payload_sha256'])
            row['restore_and_verify_seconds']=time.monotonic()-started
            delete('restored')
        third=report['cycles'][2];path=base/'checkpoints/3'
        restore('clone-a',path);restore('clone-b',path)
        verify('clone-a',third['before'],third['payload_sha256']);verify('clone-b',third['before'],third['payload_sha256'])
        guest('clone-a',['/bin/sh','-c','echo clone-a > /data/branch; echo root-a > /root-branch'])
        guest('clone-b',['/bin/sh','-c','test ! -e /data/branch; test ! -e /root-branch; echo clone-b > /data/branch'])
        assert guest('clone-a',['/bin/cat','/data/branch'])=='clone-a'
        assert artifact_hashes()==original,'saved artifacts mutated during adoption or guest writes'
        report['independent_root_and_tmpfs_clones']=True
        report['saved_filestores_immutable']=True
        recapture=base/'after-restore';capture('clone-a',recapture)
        restore('clone-c',recapture)
        verify('clone-c',third['before'],third['payload_sha256'])
        assert guest('clone-c',['/bin/cat','/data/branch'])=='clone-a'
        report['checkpoint_after_restore']=True
        shutil.rmtree(base/'checkpoints');shutil.rmtree(recapture)
        for cid in ['clone-a','clone-b','clone-c']:
            before=state(cid);time.sleep(.15);assert state(cid)['counter']>before['counter']
            verify(cid,third['before'],third['payload_sha256'])
        report['artifact_deletion_survived']=True
        report['status']='passed'
    except Exception as error:
        report['status']='failed';report['error']=f'{type(error).__name__}: {error}'
        traceback.print_exc()
    finally:
        errors=[]
        for cid in list(reversed(created)):
            try:delete(cid)
            except Exception as error:errors.append(str(error))
        subprocess.run(['umount',str(base/'state/null-netns')],capture_output=True)
        if errors:
            report['cleanup_errors']=errors
        if report['status']=='passed' and not errors:
            shutil.rmtree(base)
            report['cleanup']='all owned sandboxes, artifacts and state removed'
        else:
            report['retained_diagnostics']=str(base)
        args.output.write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps({k:v for k,v in report.items() if k not in ['cycles','filestore_extent_accounting']}),flush=True)
    return 0 if report['status']=='passed' and not report.get('cleanup_errors') else 1

if __name__=='__main__':
    raise SystemExit(main())
