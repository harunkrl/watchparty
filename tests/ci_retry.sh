#!/usr/bin/env bash
# The team's documented flake protocol, codified: one re-run before red.
# A deterministic failure fails both attempts; a timing flake passes the
# second. CI runners (2 shared cores) trip the timing-sensitive pins
# ~40% of the time — see decisions.log #50/#56.
set -u
cmd=("$@")
"${cmd[@]}"
rc=$?
if [ $rc -eq 0 ]; then exit 0; fi
echo "::warning::first attempt failed (exit $rc) — single re-run per the flake protocol"
sleep 2
"${cmd[@]}"
exit $?
