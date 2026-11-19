// Browser-like workspace tabs: each tab owns a per-session history stack and the
// whole model persists per account/workspace in sessionStorage.

export type SessionTabEntry = { href: string; title: string };
export type SessionTab = { id: string; entries: SessionTabEntry[]; index: number };
export type SessionTabsState = { active: string; tabs: SessionTab[] };

const APP_PREFIX = "/app";
const MAX_STORED_TABS = 40;
const MAX_STORED_ENTRIES = 60;
const MAX_TITLE = 200;

// One-shot credentials and onboarding signals never persist into a tab's
// restorable route. OAuth-style params are dropped on sanitize and on write.
const TRANSIENT_PARAMS = new Set([
  "code", "state", "token", "access_token", "id_token", "nonce",
  "error", "error_description", "error_uri",
  "github", "welcome", "prompt", "prefillPrompt",
]);

export function sessionTabsKey(scope: string) {
  return `canter:session-tabs:v2:${scope}`;
}

export function isSessionTabHref(href: unknown): href is string {
  if (typeof href !== "string" || !href.length || href.length > 2048) return false;
  if (!href.startsWith(APP_PREFIX)) return false;
  const boundary = href.charAt(APP_PREFIX.length);
  if (boundary && boundary !== "/" && boundary !== "?" && boundary !== "#") return false;
  if (/[\s\\]/.test(href) || href.startsWith("//") || /%2e|%5c/i.test(href)) return false;
  try {
    const url = new URL(href, "https://workspace.invalid");
    if (url.pathname !== "/app" && !url.pathname.startsWith("/app/")) return false;
    if (url.pathname.split("/").includes("..")) return false;
    return true;
  } catch {
    return false;
  }
}

// Strips transient query params; returns null for unsafe or foreign routes.
export function sanitizeSessionHref(href: unknown): string | null {
  if (!isSessionTabHref(href)) return null;
  try {
    const url = new URL(href as string, "https://workspace.invalid");
    for (const key of [...url.searchParams.keys()]) {
      if (TRANSIENT_PARAMS.has(key)) url.searchParams.delete(key);
    }
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return null;
  }
}

export function createSessionTab(id: string, href = "/app", title = "Home"): SessionTab {
  return { id, entries: [{ href, title }], index: 0 };
}

export function createSessionTabs(id: string): SessionTabsState {
  return { active: id, tabs: [createSessionTab(id)] };
}

export function activeSessionTab(state: SessionTabsState): SessionTab | undefined {
  return state.tabs.find(tab => tab.id === state.active);
}

export function currentSessionEntry(state: SessionTabsState): SessionTabEntry | undefined {
  const tab = activeSessionTab(state);
  return tab?.entries[tab.index];
}

// The active tab records a route visit: a same-href visit renames the current
// slot; a different href truncates the tab's forward stack and appends. It
// never creates a tab.
export function visitSessionRoute(state: SessionTabsState, href: string, title: string): SessionTabsState {
  const tab = activeSessionTab(state);
  if (!tab) return state;
  const current = tab.entries[tab.index];
  if (current?.href === href) {
    if (current.title === title) return state;
    const entries = tab.entries.slice();
    entries[tab.index] = { href, title };
    return { ...state, tabs: state.tabs.map(item => (item === tab ? { ...item, entries } : item)) };
  }
  const entries = [...tab.entries.slice(0, tab.index + 1), { href, title }];
  return { ...state, tabs: state.tabs.map(item => (item === tab ? { ...item, entries, index: entries.length - 1 } : item)) };
}

// A distinct tab even for an identical href: plus always opens a new slot.
export function openSessionTab(state: SessionTabsState, href: string, title: string, id: string): SessionTabsState {
  return { active: id, tabs: [...state.tabs, createSessionTab(id, href, title)] };
}

// Background open (modified click): appended without changing the active tab.
export function appendSessionTab(state: SessionTabsState, href: string, title: string, id: string): SessionTabsState {
  return { ...state, tabs: [...state.tabs, createSessionTab(id, href, title)] };
}

export function selectSessionTab(state: SessionTabsState, id: string): SessionTabsState {
  if (state.active === id || !state.tabs.some(tab => tab.id === id)) return state;
  return { ...state, active: id };
}

export function canTravelSessionTab(state: SessionTabsState, delta: number): boolean {
  const tab = activeSessionTab(state);
  if (!tab) return false;
  const index = tab.index + delta;
  return index >= 0 && index < tab.entries.length;
}

export function travelSessionTab(state: SessionTabsState, delta: number): SessionTabsState {
  const tab = activeSessionTab(state);
  if (!tab || !canTravelSessionTab(state, delta)) return state;
  return { ...state, tabs: state.tabs.map(item => (item === tab ? { ...item, index: tab.index + delta } : item)) };
}

// Browser traversal lands on a URL the active tab already visited: move its
// index to the matching entry (nearest backwards match first) instead of
// pushing a duplicate.
export function travelSessionTabTo(state: SessionTabsState, href: string): SessionTabsState {
  const tab = activeSessionTab(state);
  if (!tab) return state;
  for (let index = tab.index - 1; index >= 0; index--) {
    if (tab.entries[index].href === href) return travelSessionTab(state, index - tab.index);
  }
  for (let index = tab.index + 1; index < tab.entries.length; index++) {
    if (tab.entries[index].href === href) return travelSessionTab(state, index - tab.index);
  }
  return state;
}

// Closing the active tab selects its right neighbour, else its left; closing
// an inactive tab leaves the selection alone. The last tab resets to a fresh
// Home tab rather than leaving the strip empty.
export function closeSessionTab(state: SessionTabsState, id: string, freshId: string): SessionTabsState {
  const index = state.tabs.findIndex(tab => tab.id === id);
  if (index === -1) return state;
  if (state.tabs.length === 1) {
    const tab = createSessionTab(freshId);
    return { active: freshId, tabs: [tab] };
  }
  const tabs = state.tabs.filter(tab => tab.id !== id);
  if (state.active !== id) return { ...state, tabs };
  return { active: tabs[Math.min(index, tabs.length - 1)].id, tabs };
}

function normalizeSessionTabs(value: unknown): SessionTabsState | null {
  if (!value || typeof value !== "object") return null;
  const candidate = value as { active?: unknown; tabs?: unknown };
  if (typeof candidate.active !== "string" || !Array.isArray(candidate.tabs)) return null;
  const tabs: SessionTab[] = [];
  const ids = new Set<string>();
  for (const raw of candidate.tabs.slice(0, MAX_STORED_TABS)) {
    if (!raw || typeof raw !== "object") continue;
    const item = raw as { id?: unknown; entries?: unknown; index?: unknown };
    if (typeof item.id !== "string" || !item.id || item.id.length > 80 || ids.has(item.id) || !Array.isArray(item.entries) || !item.entries.length) continue;
    const entries: SessionTabEntry[] = [];
    for (const rawEntry of item.entries.slice(0, MAX_STORED_ENTRIES)) {
      if (!rawEntry || typeof rawEntry !== "object") continue;
      const entry = rawEntry as { href?: unknown; title?: unknown };
      const clean = sanitizeSessionHref(entry.href);
      if (!clean) continue;
      entries.push({ href: clean, title: typeof entry.title === "string" && entry.title ? entry.title.slice(0, MAX_TITLE) : "Session" });
    }
    if (!entries.length) continue;
    const index = typeof item.index === "number" && Number.isFinite(item.index) ? Math.min(Math.max(Math.trunc(item.index), 0), entries.length - 1) : entries.length - 1;
    ids.add(item.id);
    tabs.push({ id: item.id, entries, index });
  }
  if (!tabs.length) return null;
  const active = tabs.some(tab => tab.id === candidate.active) ? candidate.active : tabs[0].id;
  return { active, tabs };
}

type TabStorage = Pick<Storage, "getItem" | "setItem"> | null | undefined;

function defaultStorage(): TabStorage {
  try {
    return typeof window === "undefined" ? null : window.sessionStorage;
  } catch {
    return null;
  }
}

export function readSessionTabs(scope: string, storage?: TabStorage): SessionTabsState | null {
  try {
    const store = storage === undefined ? defaultStorage() : storage;
    if (!store) return null;
    const raw = store.getItem(sessionTabsKey(scope));
    if (!raw) return null;
    return normalizeSessionTabs(JSON.parse(raw));
  } catch {
    return null;
  }
}

export function writeSessionTabs(scope: string, state: SessionTabsState, storage?: TabStorage) {
  try {
    const store = storage === undefined ? defaultStorage() : storage;
    if (!store) return;
    const clean = normalizeSessionTabs(state);
    if (clean) store.setItem(sessionTabsKey(scope), JSON.stringify(clean));
  } catch {
    // sessionStorage may be unavailable; the strip stays in memory.
  }
}
