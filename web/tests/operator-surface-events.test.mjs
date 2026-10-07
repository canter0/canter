import test from 'node:test';
import assert from 'node:assert/strict';
import { takeReviewSurface, pendingRepositoryPicker } from '../src/lib/operator-surface-events.ts';

const event = (sequence, data, runId = 'run') => ({ sequence, runId, kind: 'surface', data, createdAt: '2026-10-07T12:00:00Z' });

test('source reading and status inspection never request foreground attention', () => {
  const seen = new Set();
  const events = ['repository', 'file', 'repository-changes', 'apps', 'app', 'billing', 'activity', 'agents', 'deployment', 'change', 'vps'].map((kind, index) => event(index + 1, { kind, id: 'existing', system: 'app' }));
  assert.equal(takeReviewSurface(events, seen), null);
  assert.equal(takeReviewSurface([event(20, { kind: 'file', id: 'commit', attention: 'review' })], seen), null);
  assert.equal(seen.size, 0);
});

test('a new approval review requests attention once even after dismissal, replay, or status reads', () => {
  const seen = new Set();
  const draft = event(1, { kind: 'deployment', id: 'proposal', attention: 'review' });
  assert.equal(takeReviewSurface([draft], seen), draft.data);
  assert.equal(takeReviewSurface([draft, event(2, { ...draft.data })], seen), null);
  assert.equal(takeReviewSurface([event(3, { kind: 'deployment', id: 'proposal' })], seen), null);
  const next = event(4, { kind: 'deployment', id: 'next', attention: 'review' });
  assert.equal(takeReviewSurface([next, event(5, { kind: 'file', id: 'commit', path: 'index.html' })], seen), next.data);
});

test('historical batches are consumed without requesting attention again', () => {
  const seen = new Set();
  const history = [event(1, { kind: 'vps', id: 'old', attention: 'review' }), event(2, { kind: 'change', id: 'old', system: 'app', attention: 'review' })];
  takeReviewSurface(history, seen);
  assert.equal(takeReviewSurface(history, seen), null);
  assert.equal(takeReviewSurface([event(3, { kind: 'change', id: 'missing-system', attention: 'review' })], seen), null);
});

test('only an unresolved explicit repository input request shows the picker', () => {
  const request = event(2, { kind: 'github', attention: 'input' });
  assert.equal(pendingRepositoryPicker([event(1, { kind: 'github' })]), undefined);
  assert.equal(pendingRepositoryPicker([event(1, { kind: 'repository' }), request]), request);
  assert.equal(pendingRepositoryPicker([request, event(3, { kind: 'repository', repository: 'owner/repo' }, 'next-run')]), undefined);
  assert.equal(pendingRepositoryPicker([request, event(3, { kind: 'file', path: 'index.html' })]), undefined);
  const reconnect = event(4, { kind: 'github', attention: 'input' }, 'reconnect');
  assert.equal(pendingRepositoryPicker([request, event(3, { kind: 'repository' }), reconnect]), reconnect);
});

test('an accepted repository choice closes the picker before inspection, including after reopening', () => {
  const request = event(1, { kind: 'github', attention: 'input' });
  const queued = { ...event(2, { message: { surface: { kind: 'repository', repository: 'owner/site' } } }, 'next-run'), kind: 'queued' };
  assert.equal(pendingRepositoryPicker([request, queued]), undefined);
  assert.equal(pendingRepositoryPicker([request, { ...queued, data: { message: { content: 'How much will this cost?' } } }]), request);
});
