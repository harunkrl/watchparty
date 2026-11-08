// F11 worker2 (S3 / A11-F9): the apt step must not eat the test budget.
//
// The release gate's test job runs on ONE 20-minute budget: the apt
// install, the go gates and the whole node battery share it. #79a's
// cancel was exactly that — the apt step stalled (a slow mirror) and the
// JOB timed out at 20 minutes, so the gate never reached the tests.
// A11's CI forensics: the packing itself is far from the timeout (the
// green job: median 104s, max 474s of 1200s) — it is the shared budget
// that is fragile, not the packing.
//
// The pin: every apt-get step in every workflow carries its OWN
// timeout-minutes, so a stalled mirror dies in minutes instead of
// consuming the budget the tests need.
//
// Static: no CI run, no network.
// Usage: node tests/ci_budget.mjs

import { strict as assert } from 'node:assert';
import { readFileSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const WF = join(ROOT, '.github/workflows');
const APT_TIMEOUT_MIN = 5;

// the steps of a workflow file: a step starts at a "- " line and runs
// until the next one.
function steps(src) {
  const lines = src.split('\n');
  const out = [];
  let cur = null;
  for (const line of lines) {
    if (/^\s*-\s/.test(line)) {
      if (cur) out.push(cur);
      cur = [line];
    } else if (cur) {
      cur.push(line);
    }
  }
  if (cur) out.push(cur);
  return out;
}

let checked = 0;
const problems = [];
for (const file of readdirSync(WF).filter((f) => f.endsWith('.yml'))) {
  const src = readFileSync(join(WF, file), 'utf8');
  for (const step of steps(src)) {
    const text = step.join('\n');
    if (!/apt-get\s+(update|install)/.test(text)) continue;
    checked++;
    const m = /timeout-minutes:\s*(\d+)/.exec(text);
    if (!m) {
      problems.push(`${file}: an apt step has no timeout-minutes (a stalled mirror eats the shared job budget)`);
    } else if (Number(m[1]) > APT_TIMEOUT_MIN) {
      problems.push(`${file}: the apt step's timeout is ${m[1]}min (the budget is ${APT_TIMEOUT_MIN}min)`);
    }
  }
}

try {
  assert.ok(checked > 0, 'no apt step found — the workflows changed shape');
  assert.deepEqual(problems, [], problems.join('; '));
} catch (e) {
  console.error(`FAIL: the CI budget contract broke — ${e.message}`);
  process.exit(1);
}

console.log(`PASS: the CI budget contract holds — ${checked} apt step(s), each with its own ${APT_TIMEOUT_MIN}min timeout`);
