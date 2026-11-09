import type { ConversationDetail, OperatorEvent } from './operator-api';
import type { Wren } from './wrens';

export type PendingReply = { speaker?: Wren; status: string; createdAt: string };

/** Reconcile durable active work with historical replay and newer live events. */
export function roomPendingReplies(detail: ConversationDetail, events: OperatorEvent[]) {
  const pending = new Map<string, PendingReply>();
  const hasSnapshot = Array.isArray(detail.activeRuns) && typeof detail.runSequence === 'number';
  const boundary = hasSnapshot ? detail.runSequence! : Infinity;
  function apply(event: OperatorEvent) {
    if (!event.runId) return;
    if (event.kind === 'queued') pending.set(event.runId, { speaker: event.data.speaker as Wren | undefined, status: 'queued', createdAt: event.createdAt });
    const run = pending.get(event.runId);
    if (run && event.kind === 'working') run.status = 'running';
    if (run && event.kind === 'finished') { run.status = String(event.data.status); run.createdAt = event.createdAt; }
  }
  for (const event of events) if (event.sequence <= boundary) apply(event);
  if (hasSnapshot) {
    // Absence from a complete snapshot is authoritative even if historical
    // replay has reached queued but not yet reached the corresponding finish.
    for (const [id, run] of pending) if (run.status === 'queued' || run.status === 'running') pending.delete(id);
    for (const run of detail.activeRuns!) pending.set(run.id, { speaker: run.speaker, status: run.status, createdAt: run.createdAt });
    for (const event of events) if (event.sequence > boundary) apply(event);
  } else if (detail.run && !pending.has(detail.run.id)) {
    // Older servers have no complete snapshot. Never overwrite a newer event
    // with their representative run, which can lag behind a finished event.
    pending.set(detail.run.id, { speaker: detail.run.speaker, status: detail.run.status, createdAt: detail.messages.at(-1)?.createdAt ?? '' });
  }
  const replies = new Set(detail.messages.filter(message => message.role === 'assistant').map(message => message.runId));
  for (const id of replies) pending.delete(id);
  return pending;
}
