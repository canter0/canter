"use client";
import { Fragment, type ReactNode } from "react";
import type { Conversation, ConversationDetail, OperatorEvent, OperatorMessage, OperatorSurface } from "@/lib/operator-api";
import type { Wren } from "@/lib/wrens";
import { OperatorTurn } from "./operator-turn";
import { WrenMessageFrame } from "./wren-message-frame";
import { WrenAvatar } from "./wren-avatar";
import styles from "./operator-workspace.module.css";

export function WrenTranscript({ detail, events, wren, onSelect, conversations, inlineForRun, onReply }: {
  detail: ConversationDetail;
  events: OperatorEvent[];
  wren?: Wren;
  onSelect: (surface: OperatorSurface) => void;
  conversations: Conversation[];
  inlineForRun?: (runID: string) => ReactNode;
  onReply?: (message: OperatorMessage) => void;
}) {
  const active = detail.run && ["queued", "running"].includes(detail.run.status) ? detail.run : null;
  const answered = new Set(detail.messages.filter(message => message.role === "assistant").map(message => message.runId));
  const lastPrompt = new Map(detail.messages.filter(message => message.role === "user").map(message => [message.runId, message.id]));
  const speaker = (date?: string) => wren ? <div className={styles.roomSpeaker}>
    <WrenAvatar shape={wren.shape} palette={wren.palette} size={23}/><strong>{wren.name}</strong>
    {date ? <time dateTime={date}>{new Date(date).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</time> : null}
  </div> : undefined;
  return <>{detail.messages.map(message => {
    const runEvents = events.filter(event => event.runId === message.runId);
    // An interrupted run can have useful files and completed actions without a
    // final assistant message. Keep that work beside its original prompt even
    // after the person sends a follow-up; never attach it to a different run.
    const interrupted = message.role === "user" && lastPrompt.get(message.runId) === message.id &&
      !answered.has(message.runId) && active?.id !== message.runId &&
      runEvents.some(event => ["tool", "text"].includes(event.kind));
    return <Fragment key={message.id}>
      <WrenMessageFrame message={message} replySenderName={wren?.name} onReply={message.role === "assistant" ? onReply : undefined}>
        <OperatorTurn resultEvents={events} message={message} userOnly={message.role === "user"}
          hidePrompt={message.role === "assistant"} speaker={message.role === "assistant" ? speaker(message.createdAt) : undefined}
          answer={message.role === "assistant" ? message : undefined} events={runEvents}
          running={!!active && message.runId === active.id} onSelect={onSelect} conversations={conversations}
          inline={message.role === "assistant" ? inlineForRun?.(message.runId) : undefined}/>
      </WrenMessageFrame>
      {interrupted ? <OperatorTurn resultEvents={events} message={{ ...message, role: "assistant", content: "" }}
        hidePrompt speaker={speaker()} events={runEvents} running={false} onSelect={onSelect} conversations={conversations}
        inline={detail.run?.id !== message.runId ? <p className={styles.note}>This turn stopped before a final reply. Its completed work is kept above.</p> : undefined}/> : null}
    </Fragment>;
  })}{active && !answered.has(active.id) ? <OperatorTurn resultEvents={events}
    message={{ id: active.id, runId: active.id, role: "assistant", content: "", createdAt: "" }} hidePrompt
    speaker={speaker()} events={events.filter(event => event.runId === active.id)} running
    onSelect={onSelect} conversations={conversations}/> : null}</>;
}
