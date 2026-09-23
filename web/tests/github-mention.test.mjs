import test from 'node:test';
import assert from 'node:assert/strict';
import { contextMention, githubMention } from '../src/lib/github-mention.ts';
test('recognizes typed prefixes and repository queries at the caret', () => {
  for (const text of ['@', '@g', '@git', '@GitHub']) assert.equal(githubMention(text, text.length).query, '');
  const text = 'Inspect @github canter';
  assert.deepEqual(githubMention(text, text.length), { start: 8, end: 22, query: 'canter' });
});
test('context mentions find categories and past conversations without matching emails or selected repositories', () => {
  assert.deepEqual(contextMention('Ask @B', 6), { start: 4, end: 6, query: 'B', category: null });
  assert.deepEqual(contextMention('Inspect @github canter', 22), { start: 8, end: 22, query: 'canter', category: 'github' });
  assert.deepEqual(contextMention('@conversations ', 15), { start: 0, end: 15, query: '', category: 'conversations' });
  assert.equal(contextMention('email@example.com', 17), null);
  assert.equal(contextMention('@owner/repo', 11), null);
});
test('keeps surrounding draft text and ignores emails, selected repositories and later lines', () => {
  const text = 'Inspect @github and keep this';
  const mention = githubMention(text, 15);
  assert.equal(text.slice(0, mention.start) + '@owner/repo ' + text.slice(mention.end), 'Inspect @owner/repo  and keep this');
  for (const value of ['email@github', '@owner/repo', '@github\nHello']) assert.equal(githubMention(value, value.length), null);
});
