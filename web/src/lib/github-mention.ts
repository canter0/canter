export function githubMention(text: string, caret: number) {
  const match = /(?:^|\s)@([^\n@]*)$/.exec(text.slice(0, caret));
  if (!match) return null;
  const value = match[1];
  const prefix = "github".startsWith(value.toLowerCase());
  if (!prefix && !/^github[\t ]/i.test(value)) return null;
  return { start: caret - value.length - 1, end: caret, query: prefix ? "" : value.slice(6).trim() };
}

export function contextMention(text: string, caret: number) {
  const match = /(?:^|\s)@([^\n@]*)$/.exec(text.slice(0, caret));
  if (!match || match[1].includes("/")) return null;
  const value = match[1];
  const category = /^(github|apps|deployments|conversations)\s+(.*)$/i.exec(value);
  if (value.includes(" ") && !category) return null;
  return { start: caret - value.length - 1, end: caret, query: category ? category[2] : value, category: category?.[1].toLowerCase() ?? null };
}
