// Shared test helpers (F6/C1).
//
// stopServer kills a server, WAITS for its exit and only then removes the
// temp dir. The server writes its room state into that dir on exit
// (state.json + state.json.tmp): removing the dir first races that write
// (ENOTEMPTY) or loses the room state silently (the write hits a missing
// dir). The suites that wait for the exit never flaked; the ones that did
// not, did.

import { rmSync } from 'node:fs';

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
      sleepSync(50 * (i + 1));
    }
  }
  rmSync(dir, { recursive: true, force: true }); // the last attempt fails loudly
}

// stopServer: kill → wait for the exit → remove the dir.
export async function stopServer(proc, dir) {
  if (proc && proc.exitCode === null && proc.signalCode === null) {
    proc.kill();
    await new Promise((r) => proc.on('exit', r));
  }
  rmWithRetry(dir);
}
