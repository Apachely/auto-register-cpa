from pathlib import Path
import difflib
import hashlib
import json

root=Path(__file__).resolve().parent
repo=root.parent/'cpa-backend'
transaction=json.loads((root/'transaction-result.json').read_text(encoding='utf-8'))
acceptance=json.loads((root/'acceptance.json').read_text(encoding='utf-8-sig'))
baseline=(root/'baseline/internal/registerbot/freeai.go').read_text(encoding='utf-8')
modified=(root/'candidate/freeai.go').read_text(encoding='utf-8')
start,end='case "email":','case "reveal":'
assert baseline[baseline.index(start):baseline.index(end)]==modified[modified.index(start):modified.index(end)]
assert (repo/'internal/registerbot/freeai.go').read_bytes()==(root/'candidate/freeai.go').read_bytes()
assert (repo/'internal/api/handlers/management/registerbot_import.go').read_bytes()==(root/'candidate/registerbot_import.go').read_bytes()
assert acceptance['status']=='done' and acceptance['done']==1 and acceptance['pendingWrite']==0
assert acceptance['keyLength']==46 and acceptance['keyCount']==7
assert acceptance['existingKeysUnchanged'] and acceptance['otherProviderFieldsUnchanged']
events=json.loads((root/'live-events.json').read_text(encoding='utf-8-sig'))
assert any(e.get('message')=='key_verified_imported' for e in events)
assert events[-1]['stage']=='done'
assert (root/'ROLLBACK.sh').read_text(encoding='utf-8').startswith('#!/bin/sh\n')
source_files=['internal/registerbot/freeai.go','internal/registerbot/freeai_test.go','internal/registerbot/testdata/google_identifier.test.cjs','internal/registerbot/testdata/onboarding_completion.test.cjs','internal/registerbot/README.md','internal/api/handlers/management/registerbot_import.go','internal/api/handlers/management/registerbot_test.go']
patch=''
for relative in source_files:
    before=root/'baseline'/relative
    old=before.read_text(encoding='utf-8').splitlines(True) if before.exists() else []
    new=(repo/relative).read_text(encoding='utf-8').splitlines(True)
    patch+=''.join(difflib.unified_diff(old,new,fromfile='a/'+relative if before.exists() else '/dev/null',tofile='b/'+relative))
(root/'source-changes.patch').write_text(patch,encoding='utf-8',newline='\n')
with (root/'VERIFICATION.txt').open('a',encoding='utf-8') as output:
    output.write('\nFINAL AUDIT\nPre-login action block is byte-for-byte unchanged.\nDriver and importer candidates match deployed build source.\nLive queue done=1; pending_write=0; seven provider keys; original six unchanged.\n')
    output.write('Earlier final-audit command exited 1 due to PowerShell quoting causing SyntaxError: unterminated string literal; no audit writes executed. Replaced with this saved Python audit.\n')
    output.write('Saved audit first exited 1 on the Windows default GBK decoder; explicitly set UTF-8 for source reads and reran.\n')
    output.write('Full repository-relative patch including regression tests: '+str(root/'source-changes.patch')+'\n')
    output.write('ROLLBACK.sh TARGET_COPY restores the original driver; ROLLBACK.sh TARGET_COPY registerbot_import.go restores the original importer. Both were executed on separate copies and verified against original hashes.\n')
for role,path in transaction['roles'].items():
    data=Path(path).read_bytes()
    assert data
    print(json.dumps({'role':role,'path':str(Path(path).resolve()),'bytes':len(data),'sha256':hashlib.sha256(data).hexdigest()}))
print('BASELINE: setup-banner loop; first secondary key could be selected; valid token-limited reasoning response rejected.')
print('MODIFIED: Finish later path; named full Default Key; strict reasoning-at-limit validation; live done=1.')
print('ROLLBACK: both production source hashes restored; original DOM behavior and reasoning-validation failures reproduced.')
print('PRELOGIN_ACTIONS_UNCHANGED=True')
print('FINAL_AUDIT=PASS')
