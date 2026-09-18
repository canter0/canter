export function githubMention(text: string, caret: number) {
  const match = /(?:^|\s)@([^\n@]*)$/.exec(text.slice(0, caret));
  if (!match) return null;
  const value = match[1];
  const prefix = "github".startsWith(value.toLowerCase());
  if (!prefix && !/^github[\t ]/i.test(value)) return null;
  return { start: caret - value.length - 1, end: caret, query: prefix ? "" : value.slice(6).trim() };
}
