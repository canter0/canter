import type { OperatorEvent } from "./operator-api";

/** Merge a sorted event history with a possibly replayed batch. Batch values win on sequence collisions. */
export function mergeOperatorEvents(current: OperatorEvent[], batch: OperatorEvent[]): OperatorEvent[] {
  if (!batch.length) return current;

  const updates = new Map<number, OperatorEvent>();
  for (const event of batch) updates.set(event.sequence, event);
  const incoming = [...updates.values()].sort((a, b) => a.sequence - b.sequence);
  if (incoming.length && (!current.length || current[current.length - 1].sequence < incoming[0].sequence)) {
    return [...current, ...incoming];
  }
  const merged: OperatorEvent[] = [];
  let left = 0;
  let right = 0;

  while (left < current.length && right < incoming.length) {
    const existing = current[left];
    const next = incoming[right];
    if (existing.sequence < next.sequence) {
      merged.push(existing);
      left++;
    } else if (existing.sequence > next.sequence) {
      merged.push(next);
      right++;
    } else {
      merged.push(next);
      left++;
      right++;
    }
  }
  while (left < current.length) merged.push(current[left++]);
  while (right < incoming.length) merged.push(incoming[right++]);
  return merged;
}
