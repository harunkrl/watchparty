#!/usr/bin/env bash
# The team's documented flake protocol, codified: one re-run before red.
# A deterministic failure fails both attempts; a timing flake passes the
# second. CI runners (2 shared cores) trip the timing-sensitive pins
# ~40% of the time — see decisions.log #50/#56.
#
# F6/C3: the retry must not hide the flake. BOTH attempts land in a log
# file, the warning names it, and the job summary records every suite
# that needed the retry — so "the suite leaned on the retry" is visible
# instead of being buried behind a green build.
set -u
cmd=("$@")
tag=$(basename "${cmd[0]}")
logdir="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
log="$logdir/wp-retry-$tag-$$.log"

"${cmd[@]}" 2>&1 | tee "$log"
rc=${PIPESTATUS[0]}
if [ $rc -eq 0 ]; then
  rm -f "$log" # a clean first attempt needs no evidence
  exit 0
fi
echo "::warning::first attempt failed (exit $rc) — single re-run per the flake protocol (log: $log)"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "| suite | first attempt | log |" >> "$GITHUB_STEP_SUMMARY"
  echo "| $tag | failed (exit $rc) | $log |" >> "$GITHUB_STEP_SUMMARY"
fi
sleep 2
"${cmd[@]}" 2>&1 | tee -a "$log"
rc=${PIPESTATUS[0]}
if [ $rc -eq 0 ]; then
  echo "::warning::$tag passed only after the retry — the flake log is at $log"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "| $tag | PASSED only after the retry | $log |" >> "$GITHUB_STEP_SUMMARY"
  fi
  exit 0
fi
echo "::error::$tag failed twice (exit $rc) — the log is at $log"
exit $rc
