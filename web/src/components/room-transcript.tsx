"use client";
import { displayName } from "@/lib/display-name";
import type { Conversation, ConversationDetail, OperatorEvent, OperatorMessage, OperatorSurface } from "@/lib/operator-api";
import type { Wren } from "@/lib/wrens";
import { WrenMessageFrame } from "./wren-message-frame";
import { OperatorTurn, ResponseText } from "./operator-turn";
import { WrenAvatar } from "./wren-avatar";
import { roomPendingReplies, type PendingReply } from "@/lib/room-pending-replies";
import styles from "./operator-workspace.module.css";

function Speaker({ wren }: { wren?: Wren }) {
  return wren ? <div className={styles.roomSpeaker}><WrenAvatar shape={wren.shape} palette={wren.palette} size={23} /><strong>{wren.name}</strong><span className={styles.wrenBadge}>Wren</span>{wren.role ? <span>{wren.role}</span> : null}</div> : null;
}
export function RoomTranscript({ detail, events, onSelect, conversations, onReply, ownEmail }: { detail: ConversationDetail; events: OperatorEvent[]; onSelect: (surface: OperatorSurface) => void; conversations: Conversation[]; onReply?: (message:OperatorMessage)=>void; ownEmail?:string }) {
  const replies = new Set(detail.messages.filter(message => message.role === "assistant").map(message => message.runId));
  const pending = roomPendingReplies(detail, events);
  function renderPending(id: string, run: PendingReply) {
    const message: OperatorMessage = { id, runId: id, role: "assistant", content: "", createdAt: run.createdAt };
    return <div key={id}><OperatorTurn resultEvents={events} message={message} hidePrompt speaker={<Speaker wren={run.speaker} />} events={events.filter(event => event.runId === id)} running={run.status === "running"} onSelect={onSelect} conversations={conversations} />{run.status === "queued" ? <p className={styles.roomWaiting}>Queued…</p> : run.status === "failed" ? <p className={styles.roomWaiting}>This reply failed. You can ask this Wren to try again.</p> : run.status === "cancelled" ? <p className={styles.roomWaiting}>Stopped.</p> : null}</div>;
  }
  // Stopped/failed turns keep their place when someone posts a later message.
  // Active replies remain at the live edge until their answer is persisted.
  const transcript: Array<{ id: string; createdAt: string; message?: OperatorMessage; pending?: PendingReply }> = detail.messages.map(message => ({ id: message.id, createdAt: message.createdAt, message }));
  for (const [id, run] of pending) {
    if (replies.has(id) || !["failed", "cancelled"].includes(run.status)) continue;
    if (detail.hasMore && Date.parse(run.createdAt) < Date.parse(detail.messages[0]?.createdAt ?? "")) continue;
    transcript.push({ id, createdAt: run.createdAt, pending: run });
  }
  transcript.sort((a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt));
  return <>{transcript.map(item => {
    if (item.pending) return renderPending(item.id, item.pending);
    const message = item.message!;
    if (message.role === "user" && message.originKind === "handoff") {
      const status = replies.has(message.runId) ? "completed" : pending.get(message.runId)?.status ?? message.handoffStatus;
      const label = ({ queued: "Queued", running: "Working", completed: "Answered", failed: "Failed", cancelled: "Stopped" } as Record<string, string>)[status ?? ""];
      return <section key={message.id} className={styles.roomHandoff} aria-label="Wren handoff"><header><strong>{message.senderName} → {message.speaker?.name ?? "Wren"}</strong><span>Handoff{label ? ` · ${label}` : ""}</span></header><ResponseText text={message.content} /></section>;
    }
    return <WrenMessageFrame own={message.role === "user" && !!ownEmail && message.senderName?.toLowerCase() === ownEmail.toLowerCase()} key={message.id} message={message} onReply={message.role==="assistant"||!!message.senderName&&message.senderName.toLowerCase()!==ownEmail?.toLowerCase()?onReply:undefined}>{message.role === "user" ? <div key={message.id} className={styles.roomHuman} data-own={!!ownEmail && message.senderName?.toLowerCase() === ownEmail.toLowerCase()}><div className={styles.humanIdentity}><span className={styles.humanAvatar} aria-hidden="true">{displayName(message.senderName).slice(0,1).toUpperCase()}</span><strong>{message.senderName?.toLowerCase() === ownEmail?.toLowerCase() ? "You" : displayName(message.senderName)}</strong><time dateTime={message.createdAt}>{new Date(message.createdAt).toLocaleTimeString([], {hour:"numeric",minute:"2-digit"})}</time></div><OperatorTurn resultEvents={events} message={message} userOnly events={[]} running={false} onSelect={onSelect} conversations={conversations} /></div> : <OperatorTurn resultEvents={events} key={message.id} message={message} hidePrompt answer={message} speaker={<Speaker wren={message.speaker} />} events={events.filter(event => event.runId === message.runId)} running={false} onSelect={onSelect} conversations={conversations} />}</WrenMessageFrame>;
  })}{[...pending].filter(([id, run]) => !replies.has(id) && ["queued", "running"].includes(run.status)).map(([id, run]) => renderPending(id, run))}</>;
}
