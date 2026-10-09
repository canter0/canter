import test from 'node:test';
import assert from 'node:assert/strict';
import { mergeConversationHistory, mergeEarlierConversationPage, normalizeConversationDetail } from '../src/lib/conversation-history.ts';

test('conversation detail pages are presented chronologically without losing their older-page cursor', () => {
  const detail = {
    conversation: { id: 'conversation' },
    messages: [{ id: 'newest' }, { id: 'older' }],
    hasMore: true,
    nextCursor: 'cursor-for-older-history',
    run: null,
  };
  const normalized = normalizeConversationDetail(detail);
  assert.deepEqual(normalized.messages.map(message => message.id), ['older', 'newest']);
  assert.equal(normalized.hasMore, true);
  assert.equal(normalized.nextCursor, 'cursor-for-older-history');
  assert.deepEqual(detail.messages.map(message => message.id), ['newest', 'older']);
});

test('a refresh preserves complete cached history when the new page advertises older rows', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [
      { id: 'old', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'shared', createdAt: '2026-01-02T00:00:00Z' },
    ],
    hasMore: false,
  };
  const incoming = {
    conversation: current.conversation,
    messages: [
      { id: 'shared', createdAt: '2026-01-02T00:00:00Z' },
      { id: 'new', createdAt: '2026-01-03T00:00:00Z' },
    ],
    hasMore: true,
    nextCursor: 'before-new-page',
  };
  const merged = mergeConversationHistory(current, incoming);
  assert.deepEqual(merged.messages.map(message => message.id), ['old', 'shared', 'new']);
  assert.equal(merged.hasMore, false);
  assert.equal(merged.nextCursor, undefined);
});

test('a disjoint recent page can reveal a gap after offline messages were added', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [{ id: 'old', createdAt: '2026-01-01T00:00:00Z' }],
    hasMore: false,
  };
  const incoming = {
    conversation: current.conversation,
    messages: [{ id: 'new', createdAt: '2026-01-03T00:00:00Z' }],
    hasMore: true,
    nextCursor: 'before-new-page',
  };
  const merged = mergeConversationHistory(current, incoming);
  assert.deepEqual(merged.messages.map(message => message.id), ['old', 'new']);
  assert.equal(merged.hasMore, true);
  assert.equal(merged.nextCursor, 'before-new-page');
});

test('a disjoint newest page moves the pagination cursor forward while retaining loaded history', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [{ id: 'old', createdAt: '2026-01-01T00:00:00Z' }],
    hasMore: true,
    nextCursor: 'older-than-loaded-history',
  };
  const incoming = {
    conversation: current.conversation,
    messages: [{ id: 'new', createdAt: '2026-01-03T00:00:00Z' }],
    hasMore: true,
    nextCursor: 'before-recent-page',
  };
  const merged = mergeConversationHistory(current, incoming);
  assert.deepEqual(merged.messages.map(message => message.id), ['old', 'new']);
  assert.equal(merged.nextCursor, 'before-recent-page');
});

test('an overlapping refresh preserves the cursor for already loaded older history', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [
      { id: 'old', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'shared', createdAt: '2026-01-02T00:00:00Z' },
    ],
    hasMore: true,
    nextCursor: 'older-than-loaded-history',
  };
  const incoming = {
    conversation: current.conversation,
    messages: [
      { id: 'shared', createdAt: '2026-01-02T00:00:00Z' },
      { id: 'new', createdAt: '2026-01-03T00:00:00Z' },
    ],
    hasMore: true,
    nextCursor: 'before-recent-page',
  };
  const merged = mergeConversationHistory(current, incoming);
  assert.deepEqual(merged.messages.map(message => message.id), ['old', 'shared', 'new']);
  assert.equal(merged.nextCursor, 'older-than-loaded-history');
});

test('message order follows timestamp instants and fractional precision', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [{ id: 'later-fraction', createdAt: '2026-01-01T00:00:00.1001Z' }],
  };
  const merged = mergeConversationHistory(current, {
    conversation: current.conversation,
    messages: [
      { id: 'earlier-fraction', createdAt: '2026-01-01T00:00:00.1Z' },
      { id: 'offset', createdAt: '2026-01-01T01:00:00+01:00' },
      { id: 'zulu-zero', createdAt: '2026-01-01T00:00:00Z' },
    ],
  });
  assert.deepEqual(merged.messages.map(message => message.id), ['offset', 'zulu-zero', 'earlier-fraction', 'later-fraction']);
});

test('loading an older page preserves the transcript, deduplicates its boundary, and advances its cursor', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [
      { id: 'shared', createdAt: '2026-01-02T00:00:00Z' },
      { id: 'new', createdAt: '2026-01-03T00:00:00Z' },
    ],
    hasMore: true,
    nextCursor: 'before-current-history',
  };
  const merged = mergeEarlierConversationPage(current, 'conversation', {
    messages: [
      { id: 'old', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'shared', createdAt: '2026-01-02T00:00:00Z' },
    ],
    hasMore: false,
  });
  assert.deepEqual(merged.messages.map(message => message.id), ['old', 'shared', 'new']);
  assert.equal(merged.hasMore, false);
  assert.equal(merged.nextCursor, undefined);
  assert.equal(mergeEarlierConversationPage(current, 'another-conversation', { messages: [], hasMore: false }), current);
});

test('loading a page from a gap keeps the full transcript in chronological order', () => {
  const current = {
    conversation: { id: 'conversation' },
    messages: [
      { id: 'old', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'newest', createdAt: '2026-01-04T00:00:00Z' },
    ],
    hasMore: true,
    nextCursor: 'before-recent-page',
  };
  const merged = mergeEarlierConversationPage(current, 'conversation', {
    messages: [
      { id: 'gap-newer', createdAt: '2026-01-03T00:00:00Z' },
      { id: 'gap-older', createdAt: '2026-01-02T00:00:00Z' },
    ],
    hasMore: true,
    nextCursor: 'before-gap',
  });
  assert.deepEqual(merged.messages.map(message => message.id), ['old', 'gap-older', 'gap-newer', 'newest']);
  assert.equal(merged.nextCursor, 'before-gap');
});
