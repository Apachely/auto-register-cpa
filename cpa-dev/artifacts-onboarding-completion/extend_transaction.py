from pathlib import Path
import difflib, hashlib, json, os, shutil, subprocess, sys

root=Path(__file__).resolve().parent
repo=root.parent/'cpa-backend'
phase=root/sys.argv[1]
phase.mkdir(exist_ok=True)
(phase/'logs').mkdir(exist_ok=True)
sha=lambda p:hashlib.sha256(p.read_bytes()).hexdigest()
production={
 'freeai.go':(repo/'internal/registerbot/freeai.go',root/'baseline/internal/registerbot/freeai.go'),
 'registerbot_import.go':(repo/'internal/api/handlers/management/registerbot_import.go',root/'baseline/internal/api/handlers/management/registerbot_import.go'),
 'settings.go':(repo/'internal/registerbot/settings.go',root/'phase1/baseline/settings.go'),
}
for name in ['gateway.go','engine.go']:
    baseline=root/'phase2/baseline'/name
    if baseline.exists(): production[name]=(repo/'internal/registerbot'/name,baseline)
if (root/'phase2/baseline/handler.go').exists():
    production['handler.go']=(repo/'internal/api/handlers/management/handler.go',root/'phase2/baseline/handler.go')
patch=''
for name,(source,baseline) in production.items():
    shutil.copyfile(source,root/'candidate'/name)
    patch+=''.join(difflib.unified_diff(baseline.read_text(encoding='utf-8').splitlines(True),source.read_text(encoding='utf-8').splitlines(True),fromfile='a/'+name,tofile='b/'+name))
(root/'DIFF_FILE.patch').write_text(patch,encoding='utf-8',newline='\n')
cases={name:base for name,(_,base) in production.items()}
for folder in ['phase1','phase2']:
    for original in (root/folder/'baseline').glob('*.go'):
        cases[folder+'-'+original.name]=original
script='''#!/bin/sh
set -eu
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  printf '%s\\n' 'Usage: ROLLBACK.sh TARGET_COPY [baseline-name]' >&2
  exit 2
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "${2:-freeai.go}" in
'''
for name,path in cases.items(): script+=f'  {name}) baseline="$script_dir/{path.relative_to(root).as_posix()}" ;;\n'
script+='''  *) printf '%s\\n' 'Unknown baseline' >&2; exit 2 ;;
esac
cat "$baseline" > "$1"
printf '%s\\n' 'RESTORED_PRISTINE_BYTES'
'''
(root/'ROLLBACK.sh').write_text(script,encoding='utf-8',newline='\n')
(root/'ROLLBACK.sh').chmod(0o755)
records=[]
def execute(name,command,expected=0,cwd=repo):
    env=dict(os.environ,REGISTERBOT_BROWSER_SMOKE='1')
    r=subprocess.run(command,cwd=cwd,env=env,capture_output=True,text=True,encoding='utf-8',errors='replace')
    records.append(dict(name=name,command=command,cwd=str(cwd),exit=r.returncode,stdout=r.stdout,stderr=r.stderr))
    (phase/'logs'/(name+'.stdout.txt')).write_text(r.stdout,encoding='utf-8')
    (phase/'logs'/(name+'.stderr.txt')).write_text(r.stderr,encoding='utf-8')
    (phase/'commands.json').write_text(json.dumps(records,indent=2),encoding='utf-8')
    assert r.returncode==expected,(name,r.returncode,r.stderr,r.stdout)
    print(name+' EXIT='+str(r.returncode))
if sys.argv[1]=='phase1':
    restore=phase/'rollback-check'
    restore.mkdir(exist_ok=True)
    for name in ['freeai.go','settings.go']:
        shutil.copyfile(root/'candidate'/name,restore/name)
        execute('RESTORE_'+name,['D:/AP/Git/bin/bash.exe',str(root/'ROLLBACK.sh'),str(restore/name),'phase1-'+name])
        assert sha(restore/name)==sha(phase/'baseline'/name)
    for label,location,expected in [('BASELINE',phase/'baseline',1),('MODIFIED',root/'candidate',0),('ROLLBACK',restore,1)]:
        overlay=phase/('overlay-'+label+'.json')
        overlay.write_text(json.dumps({'Replace':{str(repo/'internal/registerbot'/name):str(location/name) for name in ['freeai.go','settings.go']}}),encoding='utf-8')
        execute(label,['go','test','-overlay',str(overlay),'./internal/registerbot','-run','TestFreeAIFreshSurveyBrowser','-v','-count=1'],expected)
if sys.argv[1]=='phase2':
    restore=phase/'rollback-check'
    restore.mkdir(exist_ok=True)
    for name in ['freeai.go','gateway.go','settings.go','registerbot_import.go','handler.go']:
        shutil.copyfile(root/'candidate'/name,restore/name)
        execute('RESTORE_'+name,['D:/AP/Git/bin/bash.exe',str(root/'ROLLBACK.sh'),str(restore/name),'phase2-'+name])
        assert sha(restore/name)==sha(phase/'baseline'/name)
    for label,location,expected in [('BASELINE',phase/'baseline',1),('MODIFIED',root/'candidate',0),('ROLLBACK',restore,1)]:
        overlay=phase/('overlay-'+label+'.json')
        overlay.write_text(json.dumps({'Replace':{str(repo/'internal/registerbot'/name):str(location/name) for name in ['gateway.go','settings.go']}}),encoding='utf-8')
        execute(label,['go','test','-overlay',str(overlay),'./internal/registerbot','-run','TestGatewayFixedRouteAllowsParallelAccounts|TestGatewayDegradedForwardingAllowsParallelAccounts|TestEngineBatchReachesConfiguredConcurrencyThroughGateway','-v','-count=1'],expected)
    for label,location,expected in [('BASELINE',phase/'baseline',1),('MODIFIED',root/'candidate',0),('ROLLBACK',restore,1)]:
        overlay=phase/('timeout-overlay-'+label+'.json')
        overlay.write_text(json.dumps({'Replace':{str(repo/'internal/registerbot/freeai.go'):str(location/'freeai.go')}}),encoding='utf-8')
        execute('TIMEOUT_'+label,['go','test','-overlay',str(overlay),'./internal/registerbot','-run','TestFreeAIOrdinarySignInStallRetriesWithoutManualHandoff','-v','-count=1'],expected)
    for label,location in [('BASELINE',phase/'baseline'),('MODIFIED',root/'candidate'),('ROLLBACK',restore)]:
        execute('SURVEY_'+label,['node',str(repo/'internal/registerbot/testdata/onboarding_completion.test.cjs'),str(location/'freeai.go'),'--observe'])
check=phase/'patch-check'
check.mkdir(exist_ok=True)
for name,(_,baseline) in production.items():shutil.copyfile(baseline,check/name)
execute('PATCH_RECONSTRUCTION',['git','-c','core.autocrlf=false','apply','--unsafe-paths',str(root/'DIFF_FILE.patch')],cwd=check)
for name,(source,_) in production.items():
    assert sha(check/name)==sha(source)
    shutil.copyfile(check/name,root/'candidate'/name)
with (root/'VERIFICATION.txt').open('a',encoding='utf-8') as ledger:
    ledger.write('\nFOLLOW-UP '+phase.name+'\n')
    ledger.write('Target: '+str(repo)+'\n')
    for name,(source,baseline) in production.items():ledger.write(json.dumps(dict(file=str(source),baseline_sha256=sha(baseline),modified_sha256=sha(source)))+'\n')
    for record in records:ledger.write(json.dumps(record,ensure_ascii=False,indent=2)+'\n')
    ledger.write('Phase1 preparatory test compilation corrections: Pool.Close -> existing Pool.Shutdown; split invalid := selector assignment. Both initial compile commands exited 1, then substantive browser baseline reproduced operator-action failure.\n')
for name,path in dict(MODIFIED_FILE=root/'candidate/freeai.go',DIFF_FILE=root/'DIFF_FILE.patch',VERIFICATION=root/'VERIFICATION.txt',ROLLBACK=root/'ROLLBACK.sh').items():
    assert path.read_bytes()
    print(name+'='+str(path)+' SHA256='+sha(path))
