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
