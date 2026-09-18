import test from 'node:test';
import assert from 'node:assert/strict';
import { githubMention } from '../src/lib/github-mention.ts';
test('recognizes typed prefixes and repository queries at the caret', () => {
  for (const text of ['@', '@g', '@git', '@GitHub']) assert.equal(githubMention(text, text.length).query, '');
  const text = 'Inspect @github canter';
  assert.deepEqual(githubMention(text, text.length), { start: 8, end: 22, query: 'canter' });
});
test('keeps surrounding draft text and ignores emails, selected repositories and later lines', () => {
  const text = 'Inspect @github and keep this';
  const mention = githubMention(text, 15);
  assert.equal(text.slice(0, mention.start) + '@owner/repo ' + text.slice(mention.end), 'Inspect @owner/repo  and keep this');
  for (const value of ['email@github', '@owner/repo', '@github\nHello']) assert.equal(githubMention(value, value.length), null);
});
