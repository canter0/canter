import type { Wren } from './wrens';

export type WrenMentionChoice = {
  kind: 'wren' | 'team' | 'apps' | 'github' | 'conversations';
  label: string;
  detail: string;
  token: string;
  wren?: Wren;
};

export function wrenMention(text: string, caret: number, wrens: Pick<Wren, 'name'>[]) {
  const match = /(?:^|\s)@([^\n@]*)$/.exec(text.slice(0, caret));
  if (!match) return null;
  const value = match[1];
  if (value.length > 100 || value.includes('/')) return null;
  const category = /^(github|conversations)(?:\s+(.*))?$/i.exec(value);
  if (category) return { start: caret - value.length - 1, end: caret, category: category[1].toLowerCase(), query: category[2] ?? '' };
  // Keep multiword names selectable while typing, but do not cover a message
  // after a completed mention. An email address is never a mention trigger.
  const query = value.toLowerCase();
  if (/\s/.test(value) && !wrens.some(wren => wren.name.toLowerCase().startsWith(query))) return null;
  return { start: caret - value.length - 1, end: caret, category: null, query: value };
}

export function wrenMentionChoices(wrens: Wren[], shared: boolean, query: string): WrenMentionChoice[] {
  const choices: WrenMentionChoice[] = wrens.map(wren => ({ kind: 'wren', label: wren.name, detail: wren.role || 'Ask this Wren', token: wren.name, wren }));
  if (shared && wrens.length > 1) choices.push({ kind: 'team', label: 'All Wrens', detail: 'Ask the Wrens in this room', token: 'team' });
  choices.push(
    { kind: 'apps', label: 'Apps', detail: 'Connected accounts and sources', token: 'apps' },
    { kind: 'github', label: 'GitHub repositories', detail: 'Find a repository', token: 'github' },
  );
  if (!shared) choices.push({ kind: 'conversations', label: 'Earlier conversations', detail: 'Attach a conversation you can access', token: 'conversations' });
  const needle = query.trim().toLowerCase();
  return choices.filter(choice => !needle || `${choice.label} ${choice.token}`.toLowerCase().includes(needle));
}
