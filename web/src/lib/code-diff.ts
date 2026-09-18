export type DiffLine = { kind: "added" | "removed" | "context" | "hunk"; text: string; before?: number; after?: number };
export function parsePatch(patch: string): DiffLine[] {
  let before = 0, after = 0;
  return patch.split("\n").map(line => {
    const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
    if (hunk) { before = Number(hunk[1]); after = Number(hunk[2]); return { kind: "hunk", text: line }; }
    if (line.startsWith("+")) return { kind: "added", text: line.slice(1), after: after++ };
    if (line.startsWith("-")) return { kind: "removed", text: line.slice(1), before: before++ };
    if (line.startsWith(" ")) return { kind: "context", text: line.slice(1), before: before++, after: after++ };
    return { kind: "hunk", text: line };
  });
}

export type SplitDiffLine = { kind: "hunk"; text: string } | { kind: "lines"; before?: DiffLine; after?: DiffLine };
export function splitPatch(lines: DiffLine[]): SplitDiffLine[] {
  const rows: SplitDiffLine[] = [];
  let removed: DiffLine[] = [], added: DiffLine[] = [];
  const flush = () => {
    for (let i = 0; i < Math.max(removed.length, added.length); i++) rows.push({ kind: "lines", before: removed[i], after: added[i] });
    removed = []; added = [];
  };
  for (const line of lines) {
    if (line.kind === "removed") { if (added.length) flush(); removed.push(line); }
    else if (line.kind === "added") added.push(line);
    else {
      flush();
      rows.push(line.kind === "hunk" ? { kind: "hunk", text: line.text } : { kind: "lines", before: line, after: line });
    }
  }
  flush();
  return rows;
}
