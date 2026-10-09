import test from 'node:test';
import assert from 'node:assert/strict';
import { mergeOperatorEvents } from '../src/lib/operator-events.ts';

const event = (sequence, kind = 'text') => ({ sequence, kind, data: { content: `${kind}-${sequence}` }, createdAt: '2026-01-01T00:00:00Z' });

test('merges a sorted history with out-of-order replays and batch collisions', () => {
  const old = [event(1), event(3), event(5)];
  const replaced = event(3, 'updated');
  const actual = mergeOperatorEvents(old, [event(6), event(2), replaced, event(2, 'latest')]);
  assert.deepEqual(actual.map(item => item.sequence), [1, 2, 3, 5, 6]);
  assert.equal(actual[1].kind, 'latest');
  assert.equal(actual[2], replaced);
  assert.equal(actual[0], old[0]);
  assert.equal(mergeOperatorEvents(actual, []), actual);
});
