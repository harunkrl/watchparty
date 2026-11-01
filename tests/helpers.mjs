// Shared test helpers (F6/C1).
//
// stopServer kills a server, WAITS for its exit and only then removes the
// temp dir. The server writes its room state into that dir on exit
// (state.json + state.json.tmp): removing the dir first races that write
// (ENOTEMPTY) or loses the room state silently (the write hits a missing
// dir). The suites that wait for the exit never flaked; the ones that did
// not, did.

import { rmSync } from 'node:fs';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// sleepSync blocks the thread: rmSync is synchronous.
function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// rmWithRetry removes the dir, retrying the directory-level race
// (ENOTEMPTY/EBUSY): a file can appear while the tree is walked.
export function rmWithRetry(dir, tries = 3) {
  for (let i = 0; i < tries; i++) {
    try {
      rmSync(dir, { recursive: true, force: true });
      return;
    } catch (e) {
      if (e.code !== 'ENOTEMPTY' && e.code !== 'EBUSY') throw e;
      // F7: a retry is visible — a suite that needed one must not look
      // exactly like a clean run
      console.error(`tests: rm ${dir} failed (${e.code}) — retry ${i + 1}/${tries}`);
      sleepSync(50 * (i + 1));
    }
  }
  rmSync(dir, { recursive: true, force: true }); // the last attempt fails loudly
}

// stopServer: kill → wait for the exit → remove the dir.
// F7: the wait is BOUNDED. A server that ignores SIGTERM (or hangs on a
// stuck flush chain) must not hang the suite forever: 5s, then SIGKILL,
// then reap — the rm still happens.
export async function stopServer(proc, dir) {
  if (proc && proc.exitCode === null && proc.signalCode === null) {
    const exited = new Promise((r) => proc.once('exit', r));
    proc.kill();
    await Promise.race([exited, sleep(5000)]);
    if (proc.exitCode === null && proc.signalCode === null) {
      console.error(`tests: the server ${proc.pid} ignored SIGTERM — SIGKILL`);
      const killed = new Promise((r) => proc.once('exit', r));
      proc.kill('SIGKILL');
      await Promise.race([killed, sleep(2000)]);
    }
  }
  rmWithRetry(dir);
}
