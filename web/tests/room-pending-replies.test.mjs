import test from 'node:test';
import assert from 'node:assert/strict';
import { roomPendingReplies } from '../src/lib/room-pending-replies.ts';

const at = '2026-10-29T22:00:00Z';
const run = (id, status = 'running') => ({ id, status, speaker: { id: `wren_${id}`, name: id }, createdAt: at });
const detail = (activeRuns, runSequence = 20) => ({ messages: [], run: activeRuns[0] ?? null, activeRuns, runSequence });
const event = (sequence, runId, kind, data = {}) => ({ sequence, runId, kind, data, createdAt: at });

test('fresh room shows all active Wrens before any event history is available', () => {
  const pending = roomPendingReplies(detail([run('writer'), run('researcher'), run('reviewer', 'queued')]), []);
  assert.deepEqual([...pending].map(([id,r])=>[id,r.status,r.speaker.name]), [['writer','running','writer'],['researcher','running','researcher'],['reviewer','queued','reviewer']]);
});
test('partial old replay cannot revive finished work or regress a running snapshot to queued', () => {
  const pending = roomPendingReplies(detail([run('writer')]), [event(1,'old','queued'), event(2,'writer','queued')]);
  assert.deepEqual([...pending.keys()], ['writer']);
  assert.equal(pending.get('writer').status,'running');
});
test('newer completion and incoming work override snapshot independently', () => {
  const pending = roomPendingReplies(detail([run('writer'),run('researcher')]), [event(21,'writer','finished',{status:'cancelled'}),event(22,'new','queued',{speaker:{id:'new',name:'New'}}),event(23,'new','working')]);
  assert.equal(pending.get('writer').status,'cancelled');
  assert.equal(pending.get('researcher').status,'running');
  assert.equal(pending.get('new').status,'running');
});
test('persisted replies remove placeholders; empty active snapshots still retain historical stops', () => {
  const value = detail([]);
  value.messages = [{role:'assistant',runId:'answered'}];
  const pending = roomPendingReplies(value,[event(1,'stopped','queued'),event(2,'stopped','finished',{status:'cancelled'}),event(3,'answered','queued'),event(4,'answered','finished',{status:'completed'})]);
  assert.deepEqual([...pending.keys()],['stopped']);
});
test('older server representative does not overwrite a finished live event', () => {
  const value={messages:[],run:run('writer')};
  assert.equal(roomPendingReplies(value,[event(1,'writer','queued'),event(2,'writer','finished',{status:'failed'})]).get('writer').status,'failed');
  assert.equal(roomPendingReplies(value,[]).get('writer').status,'running');
});
