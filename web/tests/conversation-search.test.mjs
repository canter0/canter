import test from 'node:test';
import assert from 'node:assert/strict';
import { conversationCompletion, searchConversations } from '../src/lib/conversation-search.ts';

const conversation = (title, updatedAt = '2026-09-28T12:00:00Z') => ({ id: title, title, updatedAt, workspaceId: 'test', status: 'completed' });
const titles = (items, query) => searchConversations(items.map(item => typeof item === 'string' ? conversation(item) : item), query).map(item => item.title);

test('ranks exact, title prefix, word prefix and abbreviations in that order', () => {
  assert.deepEqual(titles(['Clock', 'Deploy code', 'Coding', 'co'], 'CO'), ['co', 'Coding', 'Deploy code', 'Clock']);
});

test('finds multiword queries, accents and short abbreviations', () => {
  assert.deepEqual(titles(['Plan a provider-neutral VPS', 'Ontario capital city', 'Café deployment'], 'vps plan'), ['Plan a provider-neutral VPS']);
  assert.deepEqual(titles(['Café deployment'], 'cafe'), ['Café deployment']);
  assert.deepEqual(titles(['Clock', 'Billing'], 'co'), ['Clock']);
  assert.deepEqual(titles(['Clock'], 'zz'), []);
});

test('uses recency to break ties and returns recent history for a blank query', () => {
  const items = [conversation('Code old', '2026-09-01T12:00:00Z'), conversation('Code new')];
  assert.deepEqual(titles(items, 'co'), ['Code new', 'Code old']);
  assert.deepEqual(titles(items, '  '), ['Code new', 'Code old']);
  assert.equal(items[0].title, 'Code old');
});

test('completes literal prefixes without rewriting fuzzy or empty input', () => {
  assert.equal(conversationCompletion('Clock', 'CL'), 'ock');
  assert.equal(conversationCompletion('Clock', 'co'), '');
  assert.equal(conversationCompletion('Clock', ''), '');
});
