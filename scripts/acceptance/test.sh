#!/usr/bin/env bash
set -euo pipefail
: "${BUSBAR_TEST_HOOK_MODULE:?signed gate fixture was not installed}"
: "${TF_ACC:?TF_ACC must be enabled}"
log="${RUNNER_TEMP:?}/acceptance.log"
go test ./internal/provider/ -run TestAcc -v -timeout 30m 2>&1 | tee "$log"
# A green job must exercise every selected lifecycle, including the hook.
if grep -Eq -- '--- SKIP: TestAcc' "$log"; then
  echo 'acceptance tests must not skip in CI' >&2
  exit 1
fi
if ! grep -Eq -- '--- PASS: TestAccHookResource ' "$log"; then
  echo 'hook acceptance did not report a pass' >&2
  exit 1
fi
