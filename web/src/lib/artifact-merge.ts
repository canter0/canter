import type { WrenArtifactDocument } from "./wren-library";

export type ArtifactDraftValue = { text: string; rows: string[][] };
export type ArtifactMerge = { draft: ArtifactDraftValue; conflicted: boolean };
const sameRows = (left: string[][], right: string[][]) => left.length === right.length && left.every((row, i) => row.length === right[i].length && row.every((cell, j) => cell === right[i][j]));
const lines = (text: string) => text.match(/[^\n]*\n|[^\n]+$/g) ?? [];

export function artifactDraftChanged(base: WrenArtifactDocument, draft: ArtifactDraftValue): boolean {
  return base.kind === "text" ? draft.text !== (base.text ?? "") : base.kind === "csv" && !sameRows(draft.rows, base.rows ?? []);
}

// A bounded, conservative three-way merge. Treat each side's changed line
// range as one edit: uncertain/overlapping ranges are left for human review.
// This avoids quadratic diff work on documents up to the editor's 1 MiB limit.
function editRange(base: string[], changed: string[]) {
  let start = 0, suffix = 0;
  while (start < base.length && start < changed.length && base[start] === changed[start]) start++;
  while (suffix < base.length - start && suffix < changed.length - start && base[base.length - suffix - 1] === changed[changed.length - suffix - 1]) suffix++;
  return { start, end: base.length - suffix, content: changed.slice(start, changed.length - suffix) };
}

export function suggestArtifactMerge(base: WrenArtifactDocument, draft: ArtifactDraftValue, latest: WrenArtifactDocument): ArtifactMerge {
  const conflict = { draft, conflicted: true };
  if (base.kind !== latest.kind || base.kind === "unsupported") return conflict;
  if (base.kind === "text") {
    const before = base.text ?? "", after = latest.text ?? "";
    if (draft.text === before || draft.text === after) return { draft: { ...draft, text: after }, conflicted: false };
    if (after === before) return { draft, conflicted: false };
    const original = lines(before), ours = editRange(original, lines(draft.text)), theirs = editRange(original, lines(after));
    const [first, second] = ours.start <= theirs.start ? [ours, theirs] : [theirs, ours];
    // Inserts at the same position or at another edit's boundary have an
    // ambiguous ordering. Adjacent replacements, however, are independent.
    if (first.end > second.start || first.end === second.start && (first.start === first.end || second.start === second.end)) return conflict;
    const text = original.slice(0, first.start).concat(first.content, original.slice(first.end, second.start), second.content, original.slice(second.end)).join("");
    return { draft: { ...draft, text }, conflicted: false };
  }
  const before = base.rows ?? [], after = latest.rows ?? [];
  if (sameRows(draft.rows, before) || sameRows(draft.rows, after)) return { draft: { ...draft, rows: after }, conflicted: false };
  if (sameRows(after, before)) return { draft, conflicted: false };
  // Row/column insertion can change cell identity. Never guess that mapping.
  if (before.length !== draft.rows.length || before.length !== after.length || before.some((row, i) => row.length !== draft.rows[i].length || row.length !== after[i].length)) return conflict;
  let conflicted = false;
  const rows = before.map((row, i) => {
    const ours = draft.rows[i], theirs = after[i];
    if (sameRows([ours], [row]) || sameRows([ours], [theirs])) return theirs;
    if (sameRows([theirs], [row])) return ours;
    // CSV has no stable row IDs. A renamed/moved row and an edit to another
    // cell in that row must not silently combine into the wrong person's data.
    conflicted = true;
    return ours;
  });
  return conflicted ? conflict : { draft: { ...draft, rows }, conflicted: false };
}
