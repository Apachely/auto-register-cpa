#!/bin/sh
set -eu
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  printf '%s\n' 'Usage: ROLLBACK.sh TARGET_COPY [baseline-name]' >&2
  exit 2
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "${2:-freeai.go}" in
  freeai.go) baseline="$script_dir/baseline/internal/registerbot/freeai.go" ;;
  registerbot_import.go) baseline="$script_dir/baseline/internal/api/handlers/management/registerbot_import.go" ;;
  settings.go) baseline="$script_dir/phase1/baseline/settings.go" ;;
  gateway.go) baseline="$script_dir/phase2/baseline/gateway.go" ;;
  engine.go) baseline="$script_dir/phase2/baseline/engine.go" ;;
  handler.go) baseline="$script_dir/phase2/baseline/handler.go" ;;
  phase1-freeai.go) baseline="$script_dir/phase1/baseline/freeai.go" ;;
  phase1-freeai_test.go) baseline="$script_dir/phase1/baseline/freeai_test.go" ;;
  phase1-settings.go) baseline="$script_dir/phase1/baseline/settings.go" ;;
  phase2-browser_gateway_test.go) baseline="$script_dir/phase2/baseline/browser_gateway_test.go" ;;
  phase2-engine.go) baseline="$script_dir/phase2/baseline/engine.go" ;;
  phase2-engine_test.go) baseline="$script_dir/phase2/baseline/engine_test.go" ;;
  phase2-freeai.go) baseline="$script_dir/phase2/baseline/freeai.go" ;;
  phase2-gateway.go) baseline="$script_dir/phase2/baseline/gateway.go" ;;
  phase2-handler.go) baseline="$script_dir/phase2/baseline/handler.go" ;;
  phase2-registerbot_import.go) baseline="$script_dir/phase2/baseline/registerbot_import.go" ;;
  phase2-registerbot_test.go) baseline="$script_dir/phase2/baseline/registerbot_test.go" ;;
  phase2-settings.go) baseline="$script_dir/phase2/baseline/settings.go" ;;
  *) printf '%s\n' 'Unknown baseline' >&2; exit 2 ;;
esac
cat "$baseline" > "$1"
printf '%s\n' 'RESTORED_PRISTINE_BYTES'
