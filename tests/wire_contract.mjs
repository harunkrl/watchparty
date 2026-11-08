// F11 worker2 (S1 / A11-F4): the WIRE_TYPES contract pin.
//
// server.js's WIRE_TYPES is a HAND-DERIVED mirror of the Go client's
// room.Event. A field added to one side only re-opens the M2-04 class
// silently: the server relays a body the Go client cannot decode, so the
// client drops the WHOLE event (room.go: the decode error skips it) ->
// the room advances and the clients miss it.
//
// The pin parses BOTH sides from the source text and asserts:
//   1. the field-name sets are EQUAL (no drift in either direction)
//   2. the type kinds match (string/number/boolean/strings)
//
// The parser is LOUD by design (master-2's F11 review): every non-empty,
// non-comment line of the struct body must be a field the pin
// understands, and every Go type must map to a known wire kind. A line
// the pin cannot read is a FAILURE, never a silent skip: an untagged
// field (Go decodes it by its field name) or an inline struct would
// otherwise slip through the contract unnoticed.
//
// Placement: a node suite (tests/ is this lane's; a Go test would live
// in worker1's lane). No server, no mpv, no client binary.
//
// Usage: node tests/wire_contract.mjs [serverJsPath] [roomGoPath]
//        (the path overrides exist for the deliberate-drift transcripts:
//         they run against COPIES, so a drift demo never touches the
//         other lane's file)

import { strict as assert } from 'node:assert';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const SERVER_JS = process.argv[2] || join(ROOT, 'server.js');
const ROOM_GO = process.argv[3] || join(ROOT, 'client-go/internal/room/room.go');

// the Go type -> the wire kind the server must expect.
// null = unknown: the pin must not guess (an unknown type is a loud fail).
function goKind(type) {
  const bare = type.replace(/^\*/, '');
  if (bare === 'string') return 'string';
  if (bare === 'bool') return 'boolean';
  if (/^(float|int|uint)/.test(bare)) return 'number';
  if (bare === '[]string') return 'strings';
  return null;
}

// server.js: the WIRE_TYPES map, read from the source text.
// The commented-out entries are NOT live: a comment line is skipped
// before the parse (master-2's nit: a commented-out entry used to count
// as a live field and produced a false RED).
function parseWireTypes(src) {
  const start = src.indexOf('const WIRE_TYPES = {');
  assert.notEqual(start, -1, 'server.js: WIRE_TYPES not found');
  const end = src.indexOf('\n};', start);
  assert.notEqual(end, -1, 'server.js: WIRE_TYPES has no closing brace');
  const out = {};
  for (const raw of src.slice(start, end).split('\n')) {
    const line = raw.replace(/\/\/.*$/, '').trim(); // the comments are not code
    if (!line) continue;
    for (const m of line.matchAll(/(\w+)\s*:\s*'(\w+)'/g)) out[m[1].toLowerCase()] = m[2];
  }
  return out;
}

// room.go: the Event struct's json tags + their Go types.
// A field WITHOUT a json tag is still decoded (encoding/json matches the
// JSON key against the FIELD NAME, case-insensitively) -> the pin counts
// it, keyed by the field name.
function parseEventStruct(src) {
  const start = src.indexOf('type Event struct {');
  assert.notEqual(start, -1, 'room.go: the Event struct not found');
  const end = src.indexOf('\n}', start);
  assert.notEqual(end, -1, 'room.go: the Event struct has no closing brace');
  const body = src.slice(src.indexOf('\n', start) + 1, end); // after the declaration line
  const out = {};
  for (const raw of body.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('//')) continue;
    const m = /^(\w+)\s+(.+?)(?:\s+`json:"([^"]*)"`)?(?:\s*\/\/.*)?$/.exec(line);
    if (!m) {
      throw new Error(`room.go: the Event struct has a line the pin does not understand: "${line}"`);
    }
    const [, name, type, tag] = m;
    const key = (tag === undefined ? name : tag.split(',')[0]).toLowerCase();
    if (key === '-') continue; // json:"-" is not on the wire
    const kind = goKind(type);
    if (kind === null) {
      throw new Error(`room.go: the pin has no wire kind for the Go type "${type}" (field ${name}) — decide it, do not skip it`);
    }
    out[key] = kind;
  }
  return out;
}

try {
  const wire = parseWireTypes(readFileSync(SERVER_JS, 'utf8'));
  const evt = parseEventStruct(readFileSync(ROOM_GO, 'utf8'));

  const wireFields = Object.keys(wire).sort();
  const evtFields = Object.keys(evt).sort();

  // the client has a field the server does not gate: the server relays
  // it AS-IS (the wire law) and the client drops the whole event on a
  // wrong type -> the M2-04 class again.
  const ungated = evtFields.filter((f) => !(f in wire));
  assert.deepEqual(ungated, [], `the client decodes fields the server does not gate: ${ungated.join(', ')} — add them to server.js WIRE_TYPES`);

  // the server gates a field the client does NOT decode: the server
  // drops it (the type gate) or relays it (unknown) -> the client's
  // struct and the server's map describe different events.
  const unknown = wireFields.filter((f) => !(f in evt));
  assert.deepEqual(unknown, [], `the server gates fields the client does not decode: ${unknown.join(', ')} — they are not in room.Event`);

  // the kinds must match: a wrong kind makes the client drop the event
  const kinds = wireFields
    .filter((f) => f in evt && wire[f] !== evt[f])
    .map((f) => `${f}: server=${wire[f]} client=${evt[f]}`);
  assert.deepEqual(kinds, [], `the wire kinds drifted: ${kinds.join(', ')}`);

  console.log(`PASS: the wire contract holds — ${wireFields.length} fields, the sets are equal and the kinds match`);
} catch (e) {
  console.error(`FAIL: the wire contract broke — ${e.message}`);
  process.exit(1);
}
