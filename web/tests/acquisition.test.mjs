import test from 'node:test';
import assert from 'node:assert/strict';
import { acquisitionSource } from '../src/lib/acquisition.ts';

test('attributes search engines without retaining search terms or matching lookalike hosts', () => {
  for (const [referrer, source] of [
    ['https://www.google.ca/search?q=private', 'google'],
    ['https://www.bing.com/search?q=private', 'bing'],
    ['https://duckduckgo.com/?q=private', 'duckduckgo'],
    ['https://search.yahoo.com/search?p=private', 'yahoo'],
    ['https://google.com.attacker.test/?token=secret', 'referral'],
    ['https://www.canter.dev/pricing', 'direct'],
    ['https://user:secret@google.com', 'direct'],
    ['invalid', 'direct'],
    ['', 'direct'],
  ]) assert.equal(acquisitionSource(referrer, 'https://canter.dev/'), source);
});

test('paid clicks are not counted as organic and raw campaign strings are not returned', () => {
  assert.equal(acquisitionSource('https://google.com', 'https://canter.dev/?gclid=secret'), 'paid');
  assert.equal(acquisitionSource('https://google.com', 'https://canter.dev/?utm_medium=cpc'), 'paid');
  assert.equal(acquisitionSource('', 'https://canter.dev/?utm_medium=email&utm_campaign=private'), 'campaign');
  assert.equal(acquisitionSource('', 'https://canter.dev/?utm_medium=secret'), 'direct');
});
