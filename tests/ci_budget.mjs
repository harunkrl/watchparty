// F11 worker2 (S3 / A11-F9): the apt step must not eat the test budget
// — and it must be a STEP, not a line inside a run block.
//
// The release gate's test job runs on ONE 20-minute budget: the apt
// install, the go gates and the whole node battery share it. #79a's
// cancel was exactly that — the apt step stalled (a slow mirror) and the
// JOB timed out at 20 minutes, so the gate never reached the tests.
// A11's CI forensics: the packing itself is far from the timeout (the
// green job: median 104s, max 474s of 1200s) — it is the shared budget
// that is fragile, not the packing.
//
// THE STRUCTURE MATTERS (the v1.9.3 hotfix): a "timeout-minutes: 5"
// line written INSIDE a `run: |` block is a SHELL LINE — the runner
// says "command not found" and the step fails (the release RED). The
// first version of this pin only checked that the TEXT existed, so it
// passed the broken shape. The pin is structural now:
//   1. a "timeout-minutes:" line inside a run block is a FAILURE,
//   2. outside a run block it must sit at the STEP-key indent (8) or
//      the JOB-key indent (4) — never deeper,
//   3. every apt step carries its own step-level timeout (an apt inside
//      a run block is fine — the step's own budget covers it; what is
//      NOT fine is a step that installs without a budget),
//   4. the release job's pack-tools install is its OWN step.
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
const STEP_KEY_INDENT = 8; // a step's keys ("timeout-minutes: 5")
const JOB_KEY_INDENT = 4; // a job's keys ("timeout-minutes: 20")

const indentOf = (line) => line.length - line.trimStart().length;

// the run blocks: a "run: |" line opens one, its content is every
// following line indented at least two spaces deeper.
function runBlocks(lines) {
  const blocks = [];
  let open = null;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (open) {
      if (line.trim() === '' || indentOf(line) >= open.base) continue;
      open.end = i - 1;
      blocks.push(open);
      open = null;
    }
    const m = /^(\s*)run:\s*\|/.exec(line);
    if (m) open = { start: i, end: lines.length - 1, base: m[1].length + 2 };
  }
  if (open) blocks.push(open);
  return blocks;
}

// the steps of a workflow: a step starts at a "- " line and runs until
// the next one.
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

const problems = [];
const aptSteps = [];

for (const file of readdirSync(WF).filter((f) => f.endsWith('.yml'))) {
  const src = readFileSync(join(WF, file), 'utf8');
  const lines = src.split('\n');
  const blocks = runBlocks(lines);
  const inRun = (i) => blocks.some((b) => i > b.start && i <= b.end);

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^\s*timeout-minutes:/.test(line)) {
      if (inRun(i)) {
        problems.push(`${file}:${i + 1}: timeout-minutes is INSIDE a run block — the shell runs it as a command ("command not found")`);
      } else {
        const indent = indentOf(line);
        if (indent !== STEP_KEY_INDENT && indent !== JOB_KEY_INDENT) {
          problems.push(`${file}:${i + 1}: timeout-minutes has the indent ${indent} (a step key is ${STEP_KEY_INDENT}, a job key is ${JOB_KEY_INDENT})`);
        }
      }
    }
  }

  // every apt step carries its own step-level timeout
  for (const step of steps(src)) {
    const text = step.join('\n');
    if (!/apt-get\s+(update|install)/.test(text)) continue;
    const first = step[0];
    aptSteps.push(`${file}: ${first.trim()}`);
    const m = /^\s{8}timeout-minutes:\s*(\d+)/m.exec(text);
    if (!m) {
      problems.push(`${file}: ${first.trim()} — the apt step has no own timeout (a stalled mirror eats the shared job budget)`);
    } else if (Number(m[1]) > APT_TIMEOUT_MIN) {
      problems.push(`${file}: ${first.trim()} — the apt step's timeout is ${m[1]}min (the budget is ${APT_TIMEOUT_MIN}min)`);
    }
  }
}

// the release job's pack tools are their OWN step (the hotfix's shape)
const releaseSrc = readFileSync(join(WF, 'client-go-release.yml'), 'utf8');
const packStep = steps(releaseSrc).find((s) => /p7zip-full/.test(s.join('\n')));
if (!packStep) {
  problems.push('client-go-release.yml: the pack-tools install (p7zip-full) has no step of its own');
} else if (!/^\s{8}timeout-minutes:\s*\d+/m.test(packStep.join('\n'))) {
  problems.push('client-go-release.yml: the pack-tools step has no step-level timeout');
}

try {
  assert.ok(aptSteps.length > 0, 'no apt step found — the workflows changed shape');
  assert.deepEqual(problems, [], problems.join('; '));
} catch (e) {
  console.error(`FAIL: the CI budget contract broke — ${e.message}`);
  process.exit(1);
}

console.log(`PASS: the CI budget contract holds — ${aptSteps.length} apt step(s), each a STEP with its own ${APT_TIMEOUT_MIN}min timeout`);
