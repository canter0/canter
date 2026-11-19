import test from 'node:test';
import assert from 'node:assert/strict';
import {
  appendSessionTab,
  activeSessionTab,
  travelSessionTabTo,
  canTravelSessionTab,
  closeSessionTab,
  createSessionTabs,
  currentSessionEntry,
  isSessionTabHref,
  openSessionTab,
  readSessionTabs,
  sanitizeSessionHref,
  selectSessionTab,
  sessionTabsKey,
  travelSessionTab,
  visitSessionRoute,
  writeSessionTabs,
} from '../src/lib/session-tabs.ts';
import { operatorDraftKey } from '../src/lib/operator-draft-storage.ts';

const scope = 'account:workspace';
const tab = id => `/app/conversations/${id}`;

function memoryStorage(initial = {}) {
  const map = new Map(Object.entries(initial));
  return {
    getItem: key => (map.has(key) ? map.get(key) : null),
    setItem: (key, value) => { map.set(key, String(value)); },
    dump: () => Object.fromEntries(map),
  };
}

function fresh() {
  return createSessionTabs('t1');
}

test('navigating inside a tab keeps the tab count and grows its history', () => {
  let state = fresh();
  state = visitSessionRoute(state, '/app', 'Home');
  state = visitSessionRoute(state, tab('one'), 'First session');
  state = visitSessionRoute(state, '/app/settings', 'General settings');
  assert.equal(state.tabs.length, 1);
  assert.deepEqual(activeSessionTab(state).entries.map(entry => entry.href), ['/app', tab('one'), '/app/settings']);
  assert.equal(currentSessionEntry(state).href, '/app/settings');
  assert.equal(currentSessionEntry(state).title, 'General settings');
});

test('a same-href visit renames the current slot instead of pushing', () => {
  let state = fresh();
  state = visitSessionRoute(state, tab('one'), 'Loading');
  state = visitSessionRoute(state, tab('one'), 'Resolved title');
  assert.equal(activeSessionTab(state).entries.length, 2);
  assert.equal(currentSessionEntry(state).title, 'Resolved title');
});

test('plus opens a distinct tab even for an identical href', () => {
  let state = fresh();
  state = openSessionTab(state, '/app?compose=1&draftTab=tab-x', 'Home', 't2');
  state = openSessionTab(state, '/app?compose=1&draftTab=tab-y', 'Home', 't3');
  assert.equal(state.tabs.length, 3);
  assert.equal(state.active, 't3');
  assert.deepEqual(state.tabs.map(item => item.id), ['t1', 't2', 't3']);
});

test('back and forward travel inside the active tab only', () => {
  let state = fresh();
  state = visitSessionRoute(state, '/app/system', 'Apps');
  state = visitSessionRoute(state, '/app/agents', 'Agents');
  state = appendSessionTab(state, tab('one'), 'Session', 't2');
  state = travelSessionTab(state, -1);
  assert.equal(currentSessionEntry(state).href, '/app/system');
  assert.equal(activeSessionTab(state).id, 't1');
  state = travelSessionTab(state, 1);
  assert.equal(currentSessionEntry(state).href, '/app/agents');
  assert.equal(canTravelSessionTab(state, 1), false);
  // The background tab's position is untouched.
  assert.equal(state.tabs[1].index, 0);
});

test('navigation after going back truncates only the active forward stack', () => {
  let state = fresh();
  state = visitSessionRoute(state, '/app/system', 'Apps');
  state = visitSessionRoute(state, '/app/agents', 'Agents');
  state = appendSessionTab(state, tab('one'), 'Session', 't2');
  state = travelSessionTab(state, -1);
  state = visitSessionRoute(state, '/app/customize', 'Customize');
  assert.deepEqual(activeSessionTab(state).entries.map(entry => entry.href), ['/app', '/app/system', '/app/customize']);
  // The inactive tab's entries are preserved.
  assert.equal(state.tabs[1].entries[0].href, tab('one'));
});

test('closing the active tab selects the right neighbour, else the left', () => {
  let state = fresh();
  state = appendSessionTab(state, tab('one'), 'One', 't2');
  state = appendSessionTab(state, tab('two'), 'Two', 't3');
  const middle = closeSessionTab({ ...state, active: 't2' }, 't2', 'fresh');
  assert.equal(middle.active, 't3');
  const last = closeSessionTab({ ...state, active: 't3' }, 't3', 'fresh');
  assert.equal(last.active, 't2');
  assert.deepEqual(last.tabs.map(item => item.id), ['t1', 't2']);
});

test('closing an inactive tab keeps the active selection and URL', () => {
  let state = fresh();
  state = appendSessionTab(state, tab('one'), 'One', 't2');
  state = visitSessionRoute(state, '/app/agents', 'Agents');
  const closed = closeSessionTab(state, 't2', 'fresh');
  assert.equal(closed.active, 't1');
  assert.equal(currentSessionEntry(closed).href, '/app/agents');
  assert.equal(closed.tabs.length, 1);
});

test('browser traversal moves the index to the matching entry', () => {
  let state = fresh();
  state = visitSessionRoute(state, '/app/system', 'Apps');
  state = visitSessionRoute(state, '/app/agents', 'Agents');
  state = travelSessionTab(state, -1);
  // Browser forward to the already-visited URL reselects instead of pushing.
  const forward = travelSessionTabTo(state, '/app/agents');
  assert.equal(activeSessionTab(forward).index, 2);
  assert.equal(activeSessionTab(forward).entries.length, 3);
  // An unvisited URL leaves the state for the visit push.
  assert.equal(travelSessionTabTo(state, '/app/billing'), state);
});

test('closing the sole tab resets to a fresh Home tab', () => {
  const closed = closeSessionTab(fresh(), 't1', 'fresh-1');
  assert.equal(closed.tabs.length, 1);
  assert.equal(closed.active, 'fresh-1');
  assert.deepEqual(currentSessionEntry(closed), { href: '/app', title: 'Home' });
});

test('select only changes the active tab', () => {
  let state = fresh();
  state = appendSessionTab(state, tab('one'), 'One', 't2');
  const selected = selectSessionTab(state, 't2');
  assert.equal(selected.active, 't2');
  assert.equal(selectSessionTab(selected, 't2'), selected);
  assert.equal(selectSessionTab(selected, 'missing'), selected);
});

test('restore validates structure, paths, duplicates, and clamps index', () => {
  const storage = memoryStorage({
    [sessionTabsKey(scope)]: JSON.stringify({
      active: 'gone',
      tabs: [
        { id: 't1', index: 9, entries: [{ href: '/app', title: 'Home' }, { href: 'https://evil.example', title: 'Bad' }] },
        { id: 't1', index: 0, entries: [{ href: '/app', title: 'Dupe' }] },
        { id: 't2', index: 0, entries: [{ href: '/app/../etc/passwd', title: 'Traversal' }] },
        { id: 't3', index: 0, entries: [{ href: tab('one'), title: 'One' }] },
        'garbage',
        { id: 't4', index: 0, entries: [] },
      ],
    }),
  });
  const restored = readSessionTabs(scope, storage);
  assert.equal(restored.active, 't1');
  assert.equal(restored.tabs.length, 2);
  assert.equal(restored.tabs[0].entries.length, 1);
  assert.equal(restored.tabs[0].index, 0);
  assert.equal(restored.tabs[1].id, 't3');
});

test('transient credentials are stripped from persisted and sanitized hrefs', () => {
  assert.equal(sanitizeSessionHref('/app?code=secret&state=xyz&compose=1'), '/app?compose=1');
  assert.equal(sanitizeSessionHref('/app?github=connected&compose=1&draftTab=t9'), '/app?compose=1&draftTab=t9');
  const storage = memoryStorage();
  const state = openSessionTab(fresh(), '/app?code=secret', 'Home', 't2');
  writeSessionTabs(scope, state, storage);
  assert.equal(readSessionTabs(scope, storage).tabs[1].entries[0].href, '/app');
});

test('unsafe and foreign routes are rejected', () => {
  for (const bad of ['https://evil.example/app', '//evil.example/app', '/app/../x', '/app/%2e%2e/x', '/app\\x', '/application', '/other', '', 42, null, '/app/%5cbad', 'javascript:alert(1)']) {
    assert.equal(isSessionTabHref(bad), false, String(bad));
    assert.equal(sanitizeSessionHref(bad), null, String(bad));
  }
  for (const good of ['/app', '/app?compose=1', tab('one'), '/app/settings/people', '/app/conversations/a%20b']) {
    assert.equal(isSessionTabHref(good), true, String(good));
  }
});

test('storage is scoped per account/workspace and degrades gracefully', () => {
  const storage = memoryStorage();
  writeSessionTabs('a:w1', visitSessionRoute(fresh(), tab('one'), 'One'), storage);
  writeSessionTabs('a:w2', fresh(), storage);
  assert.equal(readSessionTabs('a:w1', storage).tabs[0].entries.length, 2);
  assert.equal(readSessionTabs('a:w2', storage).tabs[0].entries.length, 1);
  assert.equal(readSessionTabs('b:w1', storage), null);
  const broken = { getItem: () => { throw new Error('denied'); }, setItem: () => { throw new Error('denied'); } };
  assert.equal(readSessionTabs('a:w1', broken), null);
  assert.doesNotThrow(() => writeSessionTabs('a:w1', fresh(), broken));
});

test('blank drafts per tab never collide with the legacy or each other', () => {
  const legacy = operatorDraftKey('account', 'workspace', undefined);
  const first = operatorDraftKey('account', 'workspace', 'new-tab:tab-aaa');
  const second = operatorDraftKey('account', 'workspace', 'new-tab:tab-bbb');
  assert.notEqual(legacy, first);
  assert.notEqual(first, second);
  const saved = operatorDraftKey('account', 'workspace', 'conv_123');
  assert.notEqual(saved, first);
  assert.notEqual(saved, legacy);
});
