"use client";
import { useEffect, useRef, type RefObject } from "react";
import type { Wren } from "@/lib/wrens";
import { wrenMentionChoices, type WrenMentionChoice } from "@/lib/wren-mentions";
import { WrenAvatar } from "./wren-avatar";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./github-mention-picker.module.css";

type Props = { wrens: Wren[]; shared: boolean; query: string; blockedConversation?: boolean; inputRef: RefObject<HTMLTextAreaElement | null>; onPick: (choice: WrenMentionChoice) => void; onClose: () => void };
export function WrenMentionPicker({ wrens, shared, query, blockedConversation, inputRef, onPick, onClose }: Props) {
  const panel = useRef<HTMLDivElement>(null);
  const choices = blockedConversation ? [] : wrenMentionChoices(wrens, shared, query);
  useEffect(() => {
    const input = inputRef.current;
    const keyboard = (event: KeyboardEvent) => {
      if (event.isComposing) return;
      if (event.key === "Escape") { event.preventDefault(); onClose(); return; }
      const items = panel.current?.querySelectorAll<HTMLButtonElement>("button[data-pick]");
      if (!items?.length) return;
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault(); items[event.key === "ArrowDown" ? 0 : items.length - 1].focus();
      }
      if (event.key === "Enter" && !event.shiftKey || event.key === "Tab" && !event.shiftKey) {
        event.preventDefault(); items[0].click();
      }
    };
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !panel.current?.contains(event.target) && event.target !== input) onClose(); };
    input?.addEventListener("keydown", keyboard); document.addEventListener("pointerdown", outside);
    return () => { input?.removeEventListener("keydown", keyboard); document.removeEventListener("pointerdown", outside); };
  }, [inputRef, onClose]);
  return <div ref={panel} id="context-mention-picker" className={styles.panel} role="dialog" aria-label="Mention a Wren or source" onKeyDown={event => {
    if (event.key === "Escape") { event.preventDefault(); onClose(); inputRef.current?.focus(); }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const items = Array.from(panel.current?.querySelectorAll<HTMLButtonElement>("button[data-pick]") ?? []);
      if (items.length) items[(items.indexOf(document.activeElement as HTMLButtonElement) + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length].focus();
    }
  }}><div className={styles.results}>
    {choices.map(choice => <button data-pick type="button" key={choice.wren?.id ?? choice.kind} onClick={() => onPick(choice)}>
      {choice.wren ? <WrenAvatar shape={choice.wren.shape} palette={choice.wren.palette} size={22}/> : <WorkspaceIcon name={choice.kind === "apps" ? "apps" : choice.kind === "github" ? "folder" : "message"}/>}
      <span>{choice.label}</span><small>{choice.detail}</small>
    </button>)}
    {!choices.length ? <p>{blockedConversation ? "Private conversations can’t be attached to a shared room. Share the relevant text or file here instead." : "No matching Wren or source."}</p> : null}
  </div></div>;
}
