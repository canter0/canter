#!/usr/bin/env python3
"""Bounded file checkpoints. Never captures RAM, processes, or host credentials."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tarfile
import time

ROOT=Path(os.environ.get('CANTER_COMPUTER_ROOT','/workspace')).resolve()
MAX_RAW=128<<20
MAX_COMPRESSED=32<<20
MAX_FILES=20000
CACHE_NAMES={'node_modules','.cache','__pycache__'}
BROWSER_CACHES={'Cache','Code Cache','GPUCache','ShaderCache','GrShaderCache','Crashpad','Crash Reports'}
BROWSER_COMPONENTS={'optimization_guide_model_store','component_crx_cache','extensions_crx_cache','WasmTtsEngine','Safe Browsing','OnDeviceHeadSuggestModel','DeferredBrowserMetrics','ActorSafetyLists','hyphen-data','ZxcvbnData','CertificateRevocation','OptimizationHints','Dictionaries','PKIMetadata','Subresource Filter','Crowd Deny','SafetyTips'}

def quiesce_browser():
    targets=[]
    proc=Path(os.environ.get('CANTER_COMPUTER_PROC','/proc'))
    if not proc.is_dir():return False
    for entry in proc.iterdir():
        if not entry.name.isdigit():continue
        try:
            command=(entry/'cmdline').read_bytes().split(b'\0')
            if command and command[0].decode(errors='replace').endswith('/chrome') and b'--user-data-dir=/workspace/.browser' in command and not any(arg.startswith(b'--type=') for arg in command):
                targets.append(int(entry.name))
        except (FileNotFoundError,PermissionError,ProcessLookupError):pass
    original=list(targets)
    if targets:
        env={**os.environ,'DISPLAY':':99'}
        windows=subprocess.run(['xdotool','search','--onlyvisible','--class','[Gg]oogle-chrome'],env=env,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5).stdout.decode().split()
        for window in windows:
            if not window.isdigit():continue
            owner=subprocess.run(['xdotool','getwindowpid',window],env=env,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5).stdout.decode().strip()
            if not owner.isdigit() or int(owner) not in targets:continue
            # Send the normal window-manager close shortcut. SIGTERM marks
            # Chromium profiles crashed and produces Restore-pages prompts.
            subprocess.run(['xdotool','windowactivate','--sync',window],env=env,check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
            subprocess.run(['xdotool','key','--clearmodifiers','alt+F4'],env=env,check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
    deadline=time.monotonic()+15
    while targets and time.monotonic()<deadline:
        targets=[pid for pid in targets if (proc/str(pid)).exists()]
        if targets:time.sleep(.2)
    if targets:raise RuntimeError('Browser did not close cleanly; running computer retained.')
    return bool(original)

def ensure_no_running_jobs(base,action):
    state=Path(os.environ.get('CANTER_COMPUTER_STATE','/home/wren/.canter'))/'jobs'
    own='checkpoint_'+action+'_'+base.name
    for folder in state.glob('*'):
        if folder.name==own:continue
        try:
            value=json.loads((folder/'state.json').read_text())
            pid=value.get('workerPid');started=value.get('workerStart')
            if value.get('status') in ('starting','running') and pid and started:
                actual=Path('/proc',str(pid),'stat').read_text().rsplit(')',1)[1].split()[19]
                if actual==started:raise RuntimeError('A computer command is still running; stop it before checkpointing.')
        except (FileNotFoundError,PermissionError,ValueError,IndexError):pass

def reopen_browser():
    browser=shutil.which('google-chrome') or shutil.which('chromium')
    if browser:subprocess.Popen([browser,'--user-data-dir=/workspace/.browser','--no-first-run','--no-default-browser-check','--password-store=basic','--start-maximized','--window-size=1280,800','--restore-last-session'],env={**os.environ,'DISPLAY':':99'},stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True)

def allowed(name,include_browser):
    parts=Path(name).parts
    if not parts or parts[0]=='.canter':return False
    if parts[0]=='.browser':
        if not include_browser:return False
        if len(parts)>1 and (parts[1] in BROWSER_COMPONENTS or parts[1].startswith('BrowserMetrics')):return False
        if any(part in BROWSER_CACHES or part.startswith('Singleton') for part in parts[1:]):return False
    return not any(part in CACHE_NAMES for part in parts)

def create(base,request):
    include=bool(request.get('includeBrowser'))
    archive=base/'workspace.tar.gz';temporary=base/'workspace.partial'
    skipped=[];entries=[];total=0
    with tarfile.open(temporary,'w:gz',compresslevel=6,dereference=False) as out:
        for directory,dirs,files in os.walk(ROOT,followlinks=False):
            relative=Path(directory).relative_to(ROOT)
            kept=[]
            for name in sorted(dirs):
                child=Path(directory)/name;name_relative=(relative/name).as_posix()
                if not allowed(name_relative,include) or (child/'pyvenv.cfg').is_file():skipped.append(name_relative);continue
                if child.is_symlink():raise RuntimeError('Checkpoint contains an unsupported symbolic link; running computer retained.')
                out.add(child,arcname=name_relative,recursive=False)
                kept.append(name)
            dirs[:]=kept
            for name in sorted(files):
                source=Path(directory)/name;name_relative=(relative/name).as_posix()
                if not allowed(name_relative,include):skipped.append(name_relative);continue
                info=source.lstat()
                if not stat.S_ISREG(info.st_mode):raise RuntimeError('Checkpoint contains an unsupported special file; running computer retained.')
                total+=info.st_size
                if total>MAX_RAW or len(entries)>=MAX_FILES:raise RuntimeError('Checkpoint exceeds the 128 MiB unpacked or 20000-file limit; running computer retained.')
                digest=hashlib.sha256()
                with source.open('rb') as stream:
                    for chunk in iter(lambda:stream.read(1<<20),b''):digest.update(chunk)
                out.add(source,arcname=name_relative,recursive=False)
                after=source.stat()
                if after.st_size!=info.st_size or after.st_mtime_ns!=info.st_mtime_ns:raise RuntimeError('A file changed during checkpoint; retry after its work stops.')
                entries.append({'path':name_relative,'size':info.st_size,'sha256':digest.hexdigest()})
                if temporary.stat().st_size>MAX_COMPRESSED:raise RuntimeError('Checkpoint exceeds the 32 MiB compressed limit; running computer retained.')
    if temporary.stat().st_size>MAX_COMPRESSED:raise RuntimeError('Checkpoint exceeds the 32 MiB compressed limit; running computer retained.')
    temporary.replace(archive)
    manifest={'format':'wren-workspace-tar-gzip-v1','includeBrowser':include,'ram':False,'processes':False,'installedSystemApps':False,'browserLoginGuaranteed':False,'rawBytes':total,'compressedBytes':archive.stat().st_size,'entries':entries,'skippedRebuildablePaths':skipped,'archiveSha256':hashlib.sha256(archive.read_bytes()).hexdigest()}
    (base/'manifest.json').write_text(json.dumps(manifest))
    print(json.dumps({'files':len(entries),'rawBytes':total,'compressedBytes':archive.stat().st_size}))

def restore(base,request,verify_only=False):
    include=bool(request.get('includeBrowser'))
    if include and not verify_only:quiesce_browser()
    archive=base/'workspace.tar.gz';staging=base/'staging';staging.mkdir(exist_ok=True)
    parts=request.get('parts',0)
    if not isinstance(parts,int) or parts<1 or parts>4:raise RuntimeError('Invalid restore part count.')
    with archive.open('wb') as target:
        for number in range(parts):
            with (base/'parts'/str(number)).open('rb') as source:
                while True:
                    chunk=source.read(1<<20)
                    if not chunk:break
                    target.write(chunk)
                    if target.tell()>MAX_COMPRESSED:raise RuntimeError('Restore data exceeds compressed limit.')
    if hashlib.sha256(archive.read_bytes()).hexdigest()!=request.get('archiveSha256'):raise RuntimeError('Restore data verification failed.')
    if archive.stat().st_size>MAX_COMPRESSED:raise RuntimeError('Checkpoint exceeds compressed limit.')
    total=0;count=0;expected={}
    with tarfile.open(archive,'r:gz') as source:
        for item in source:
            parts=Path(item.name).parts
            if item.name.startswith('/') or '..' in parts or not allowed(item.name,include) or not (item.isfile() or item.isdir()):raise RuntimeError('Checkpoint contains an unsafe member.')
            total+=item.size;count+=1
            if total>MAX_RAW or count>MAX_FILES:raise RuntimeError('Checkpoint exceeds extraction limit.')
            target=staging/item.name
            if item.isdir():target.mkdir(parents=True,exist_ok=True);continue
            target.parent.mkdir(parents=True,exist_ok=True)
            with source.extractfile(item) as reader,target.open('wb') as writer:
                digest=hashlib.sha256()
                for chunk in iter(lambda:reader.read(1<<20),b''):writer.write(chunk);digest.update(chunk)
                expected[item.name]=digest.hexdigest()
            target.chmod(item.mode&0o777&~0o6000)
    # A fresh VM's browser may already have created newer default session files.
    # Replace its profile tree atomically instead of merging stale/new sessions.
    staged_browser=staging/'.browser'
    if include and staged_browser.exists():
        target_browser=ROOT/'.browser'
        if target_browser.is_symlink():raise RuntimeError('Browser restore target is a symbolic link.')
        previous=base/'previous-browser'
        if previous.exists():shutil.rmtree(previous)
        if target_browser.exists():os.replace(target_browser,previous)
        os.replace(staged_browser,target_browser)
    for directory,dirs,files in os.walk(staging):
        relative=Path(directory).relative_to(staging)
        destination=ROOT/relative
        if destination.is_symlink() or not destination.resolve().is_relative_to(ROOT):raise RuntimeError('Restore destination escapes workspace.')
        destination.mkdir(parents=True,exist_ok=True)
        for name in files:
            target=destination/name
            if target.is_symlink():raise RuntimeError('Restore destination is a symbolic link.')
            os.replace(Path(directory)/name,target)
    for name,digest in expected.items():
        actual=hashlib.sha256()
        with (ROOT/name).open('rb') as stream:
            for chunk in iter(lambda:stream.read(1<<20),b''):actual.update(chunk)
        if actual.hexdigest()!=digest:raise RuntimeError('Restored file verification failed.')
    if include and not verify_only:reopen_browser()
    print(json.dumps({'restoredFiles':len(expected),'rawBytes':total,'contentVerified':True,'verificationOnly':verify_only,'ramRestored':False,'processesRestored':False}))

if __name__=='__main__':
    action,raw=sys.argv[1:]
    base=Path(raw).resolve()
    if base.parent!=ROOT/'.canter/checkpoints':raise RuntimeError('Invalid checkpoint scope.')
    request=json.loads((base/'request.json').read_text())
    if action=='create':
        ensure_no_running_jobs(base,action)
        was_open=quiesce_browser() if request.get('includeBrowser') else False
        success=False
        try:create(base,request);success=True
        finally:
            if was_open and (not request.get('keepBrowserStopped') or not success):reopen_browser()
    elif action=='restore':restore(base,request)
    elif action=='verify':
        ROOT=ROOT/'.canter/restore-proofs'/base.name
        ROOT.mkdir(parents=True,exist_ok=True)
        restore(base,request,True)
    else:raise RuntimeError('Unknown checkpoint operation.')
