import test from 'node:test';
import assert from 'node:assert/strict';
import { operatorActivity } from '../src/lib/operator-activity.ts';

const event = (sequence, kind, data) => ({ sequence, kind, data, createdAt: '2026-10-09T12:00:00Z', runId: 'run' });

test('activity starts quietly and follows the current tool', () => {
  assert.equal(operatorActivity([], true).label, 'Thinking');
  assert.equal(operatorActivity([event(1, 'tool', { callId: 'read', name: 'canter_read_repository_file', status: 'running' })], true).label, 'Reading source');
});

test('replayed tool updates count once and finished activity reports actions', () => {
  const events = [event(1, 'tool', { callId: 'a', status: 'running' }), event(2, 'tool', { callId: 'a', status: 'completed' }), event(3, 'tool', { callId: 'b', status: 'completed' })];
  assert.deepEqual(operatorActivity(events, false), { count: 2, failed: 0, label: '2 actions' });
  assert.equal(operatorActivity(events.slice(0, 2), false).label, '1 action');
});

test('a stale running snapshot cannot leave completed activity shimmering', () => {
  assert.equal(operatorActivity([event(1, 'tool', { callId: 'a', name: 'canter_bash', status: 'running' })], false).label, '1 action');
});

test('failures remain visible in the compact summary', () => {
  assert.deepEqual(operatorActivity([event(1, 'tool', { callId: 'a', status: 'failed' })], false), { count: 1, failed: 1, label: '1 action · 1 failed' });
});

test('writing begins after the tool and commentary is not counted as an action', () => {
  const events = [event(1, 'text', { content: 'I will check.' }), event(2, 'tool', { callId: 'a', status: 'completed' }), event(3, 'text', { content: 'Here is the result.' })];
  assert.equal(operatorActivity(events, true).label, 'Writing response');
  assert.equal(operatorActivity(events, true).count, 1);
  assert.equal(operatorActivity([event(1, 'text', { content: 'Hello' })], false).count, 0);
});
