import test from 'node:test';
import assert from 'node:assert/strict';
import { turnTimeline, elapsedLabel } from '../src/lib/operator-timeline.ts';
import { parsePatch } from '../src/lib/code-diff.ts';
const event = (sequence, kind, data) => ({ sequence, kind, data, createdAt: '2026-09-16T12:00:00Z', runId: 'run' });
test('replayed cumulative text stays beside its tools and final response is not duplicated', () => {
  const events = [event(1, 'text', {step:1, content:'I’ll'}), event(2,'text',{step:1,content:'I’ll inspect the repository.'}), event(3,'tool',{callId:'a',status:'running'}), event(4,'surface',{kind:'repository'}),event(5,'tool',{callId:'a',status:'completed'}),event(6,'text',{step:2,content:'Now reading source.'}),event(7,'tool',{callId:'b',status:'running'}),event(8,'tool',{callId:'b',status:'completed'}),event(9,'text',{step:3,content:'Ready.'})];
  const value = turnTimeline(events);
  assert.equal(value.preamble.data.content,'I’ll inspect the repository.');
  assert.deepEqual(value.work.map(row => row.kind),['tool','text','tool']);
  assert.equal(value.work[0].data.status,'completed');
  assert.equal(value.tail.data.content,'Ready.');
  assert.equal(turnTimeline(events.slice(0,3)).work[0].data.status,'running');
});
test('text-only answers remain a single response', () => {
  const value = turnTimeline([event(1,'text',{step:1,content:'Hello'}),event(2,'text',{step:1,content:'Hello there.'})]);
  assert.equal(value.preamble,null); assert.equal(value.work.length,0); assert.equal(value.tail.data.content,'Hello there.');
});
test('elapsed time remains stable after replay and supports minutes', () => {
  assert.equal(elapsedLabel('2026-09-16T12:00:00Z','2026-09-16T12:01:38Z'),'1m 38s');
});
test('diff gutters follow removed/added lines and reset at every hunk', () => {
  const rows = parsePatch('@@ -2,2 +2,3 @@\n keep\n-old\n+new\n+extra\n@@ -30 +31 @@\n-before\n+after\n\\ No newline at end of file');
  assert.deepEqual(rows.slice(1,5).map(r => [r.kind,r.before,r.after]),[['context',2,2],['removed',3,undefined],['added',undefined,3],['added',undefined,4]]);
  assert.equal(rows[6].before,30); assert.equal(rows[7].after,31); assert.equal(rows[8].kind,'hunk');
});
