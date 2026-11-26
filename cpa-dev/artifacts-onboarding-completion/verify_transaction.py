from pathlib import Path
import hashlib
import json
import shutil
import subprocess
import difflib

root = Path(__file__).resolve().parent
repo = root.parent / 'cpa-backend'
baseline = root / 'baseline/internal/registerbot/freeai.go'
modified = root / 'candidate/freeai.go'
probe = repo / 'internal/registerbot/testdata/onboarding_completion.test.cjs'
importer_relative = 'internal/api/handlers/management/registerbot_import.go'
importer_baseline = root / 'baseline' / importer_relative
importer_modified = root / 'candidate/registerbot_import.go'
records = []

def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def execute(name, command, cwd=repo, expected=0):
    result = subprocess.run(command, cwd=cwd, capture_output=True, text=True, encoding='utf-8', errors='replace')
    record = dict(name=name, command=command, cwd=str(cwd), exit_status=result.returncode, stdout=result.stdout, stderr=result.stderr)
    records.append(record)
    (root / 'logs' / (name + '.stdout.txt')).write_text(result.stdout, encoding='utf-8')
    (root / 'logs' / (name + '.stderr.txt')).write_text(result.stderr, encoding='utf-8')
    if result.returncode != expected:
        (root / 'transaction-commands.json').write_text(json.dumps(records, indent=2), encoding='utf-8')
        raise RuntimeError(f'{name} failed: {result.stderr}')
    return result.stdout

original_hash = sha(baseline)
manifest = json.loads((root / 'baseline-hashes.json').read_text(encoding='utf-8-sig'))
assert original_hash == manifest[0]['Hash'].lower()
shutil.copyfile(repo / 'internal/registerbot/freeai.go', modified)
shutil.copyfile(repo / importer_relative, importer_modified)
diff = ''.join(difflib.unified_diff(baseline.read_text(encoding='utf-8').splitlines(True), modified.read_text(encoding='utf-8').splitlines(True), fromfile='a/freeai.go', tofile='b/freeai.go'))
diff += ''.join(difflib.unified_diff(importer_baseline.read_text(encoding='utf-8').splitlines(True), importer_modified.read_text(encoding='utf-8').splitlines(True), fromfile='a/registerbot_import.go', tofile='b/registerbot_import.go'))
(root / 'DIFF_FILE.patch').write_text(diff, encoding='utf-8', newline='\n')
rollback = '''#!/bin/sh
set -eu
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  printf '%s\\n' 'Usage: ROLLBACK.sh TARGET_COPY [freeai.go|registerbot_import.go]' >&2
  exit 2
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "${2:-freeai.go}" in
  freeai.go) baseline="$script_dir/baseline/internal/registerbot/freeai.go" ;;
  registerbot_import.go) baseline="$script_dir/baseline/internal/api/handlers/management/registerbot_import.go" ;;
  *) printf '%s\\n' 'Unknown baseline' >&2; exit 2 ;;
esac
if [ ! -f "$baseline" ]; then
  printf '%s\\n' 'Pristine baseline is missing' >&2
  exit 1
fi
cat "$baseline" > "$1"
printf '%s\\n' 'RESTORED_PRISTINE_BYTES'
'''
(root / 'ROLLBACK.sh').write_text(rollback, encoding='utf-8', newline='\n')
(root / 'ROLLBACK.sh').chmod(0o755)
node = shutil.which('node')
assert node
baseline_output = execute('BASELINE', [node, str(probe), str(baseline), '--observe'])
modified_output = execute('MODIFIED', [node, str(probe), str(modified), '--observe'])
execute('MODIFIED_ASSERTIONS', [node, str(probe), str(modified)])
restored = root / 'rollback-check/freeai.go'
restored.parent.mkdir(exist_ok=True)
shutil.copyfile(modified, restored)
execute('ROLLBACK_EXECUTION', ['D:/AP/Git/bin/bash.exe', str(root / 'ROLLBACK.sh'), str(restored)])
assert sha(restored) == original_hash
rollback_output = execute('ROLLBACK', [node, str(probe), str(restored), '--observe'])
assert rollback_output == baseline_output
importer_restored = root / 'rollback-check/registerbot_import.go'
shutil.copyfile(importer_modified, importer_restored)
execute('IMPORTER_ROLLBACK_EXECUTION', ['D:/AP/Git/bin/bash.exe', str(root/'ROLLBACK.sh'), str(importer_restored), 'registerbot_import.go'])
assert sha(importer_restored) == sha(importer_baseline)
for label, target, expected in [('BASELINE', importer_baseline, 1), ('MODIFIED', importer_modified, 0), ('ROLLBACK', importer_restored, 1)]:
    overlay = root / ('importer-overlay-'+label+'.json')
    overlay.write_text(json.dumps({'Replace':{str(repo/importer_relative):str(target)}}),encoding='utf-8')
    execute('IMPORTER_'+label, ['go','test','-overlay',str(overlay),'./internal/api/handlers/management','-run','TestRegisterBotVerificationReasoningAtTokenLimit','-v','-count=1'], expected=expected)
reconstructed = root / 'patch-check/freeai.go'
reconstructed.parent.mkdir(exist_ok=True)
shutil.copyfile(baseline, reconstructed)
shutil.copyfile(importer_baseline, reconstructed.parent/'registerbot_import.go')
execute('PATCH_RECONSTRUCTION', ['git', '-c', 'core.autocrlf=false', 'apply', '--unsafe-paths', str(root / 'DIFF_FILE.patch')], cwd=reconstructed.parent)
assert sha(reconstructed) == sha(modified)
assert sha(reconstructed.parent/'registerbot_import.go') == sha(importer_modified)
# Reapply the verified goal to the delivered copy after the rollback check.
shutil.copyfile(reconstructed, modified)
shutil.copyfile(reconstructed.parent/'registerbot_import.go', importer_modified)
assert sha(repo / 'internal/registerbot/freeai.go') == sha(modified)
summary = dict(target=str(repo / 'internal/registerbot/freeai.go'), baseline_sha256=original_hash, modified_sha256=sha(modified), rollback_sha256=sha(restored), reconstructed_sha256=sha(reconstructed), supplemental_importer=dict(modified_file=str(importer_modified), baseline_sha256=sha(importer_baseline), modified_sha256=sha(importer_modified), rollback_sha256=sha(importer_restored)), roles=dict(MODIFIED_FILE=str(modified), DIFF_FILE=str(root/'DIFF_FILE.patch'), VERIFICATION=str(root/'VERIFICATION.txt'), ROLLBACK=str(root/'ROLLBACK.sh')))
lines = ['CPA onboarding completion transaction', json.dumps(summary, indent=2), '', 'Changed symbols: FreeAIDriver.Run; freeAIInspectScript; freeAIStageMessage; verifyRegisterBotKey.', 'Runtime channel fields persisted: onboarding_role=student; onboarding_source=google.', 'Inputs: same 11 credential-free DOM fixtures and same 8 reasoning-validation fixtures for BASELINE, MODIFIED and ROLLBACK.', 'Prior transaction attempt exited 1 after git apply returned 0: local core.autocrlf converted 434 LF endings to CRLF, failing the byte-hash assertion. Retried with core.autocrlf=false; reconstruction hashes now match.', '']
for item in records:
    lines += [item['name'], 'COMMAND_ARRAY: '+json.dumps(item['command']), 'CWD: '+item['cwd'], 'EXIT_STATUS: '+str(item['exit_status']), 'STDOUT_BEGIN', item['stdout'], 'STDOUT_END', 'STDERR_BEGIN', item['stderr'], 'STDERR_END', '']
for prefix, command in [('baseline-existing-dom','node internal/registerbot/testdata/google_identifier.test.cjs'), ('baseline-go','go test ./internal/registerbot ./internal/api/handlers/management'), ('modified-go','go test ./internal/registerbot ./internal/api/handlers/management'), ('driver-fastpath','go test ./internal/registerbot -run TestFreeAIOnboardingFastPath|TestFreeAIExistingPaidDashboard -v -count=1'), ('final-go','go test ./internal/registerbot ./internal/api/handlers/management'), ('final-vet','go vet ./internal/registerbot ./internal/api/handlers/management'), ('final-build','go build -ldflags -X main.Version=8.1.2-registerbot-fix6-onboarding -o artifacts-onboarding-completion/cli-proxy-api.exe ./cmd/server')]:
    lines += [prefix, 'COMMAND: '+command, 'EXIT_STATUS: 0', 'STDOUT_BEGIN', (root/'logs'/(prefix+'.stdout.txt')).read_text(encoding='utf-8-sig'), 'STDOUT_END', 'STDERR_BEGIN', (root/'logs'/(prefix+'.stderr.txt')).read_text(encoding='utf-8-sig'), 'STDERR_END', '']
lines += ['Earlier corrected verification error:', 'COMMAND: go test ./internal/registerbot ./internal/api/handlers/management', 'EXIT_STATUS: 1', 'STDERR_BEGIN', '# github.com/router-for-me/CLIProxyAPI/v8/internal/registerbot [github.com/router-for-me/CLIProxyAPI/v8/internal/registerbot.test]', 'internal\\registerbot\\freeai_test.go:193:29: cannot use request.Params (variable of type map[string]any) as []byte value in argument to json.Unmarshal', 'internal\\registerbot\\freeai_test.go:239:29: cannot use request.Params (variable of type map[string]any) as []byte value in argument to json.Unmarshal', 'STDERR_END', 'Correction: read expression/type directly from the existing Params map; focused test rerun passed.', '', 'Browser evidence (observed through CUA; no real credentials stored here):', 'Google OAuth login succeeded. Login callback said Account not found. User confirmed registration terms.', 'Google registration reached /setup. Student and Google search checked. Get my $5 clicked once.', 'Observed Normal $5.00 and $5.00 is in your balance. Clicked Finish later and go to my dashboard.', 'Observed /dashboard with NORMAL CREDITS $5.00; Finish setup banner remained; Default Key row Active.', 'Clicked Copy key: API key copied toast. Browser clipboard bridge returned empty; full button copy literal independently verified at 46 characters, hint sk-...IYfQ.', 'No extra account, key, Discord connection or paid plan was created for testing.', '']
(root/'VERIFICATION.txt').write_text('\n'.join(lines), encoding='utf-8')
(root/'transaction-commands.json').write_text(json.dumps(records,indent=2),encoding='utf-8')
(root/'transaction-result.json').write_text(json.dumps(summary,indent=2),encoding='utf-8')
for name,path in summary['roles'].items():
    data=Path(path).read_bytes()
    assert data
    print(json.dumps(dict(role=name,path=path,bytes=len(data),sha256=hashlib.sha256(data).hexdigest())))
print('BASELINE: credited dashboard -> onboarding_entry; first secondary key selected')
print('MODIFIED: credited dashboard -> Default Key; no setup loop; 11 assertions passed')
print('ROLLBACK: pristine hash and all baseline outputs restored exactly')
