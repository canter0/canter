"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useEffect, useId, useRef, useState, type FormEvent } from "react";
import { createPortal } from "react-dom";
import { canterFetch } from "@/lib/canter-api";
import { conversationBase, type Conversation } from "@/lib/operator-api";
import { clearOperatorAttachmentDraft } from "@/lib/operator-attachment-draft";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./conversation-list.module.css";

type Selection = { conversation: Conversation; trigger: HTMLButtonElement };

export function ConversationList({ compact = false }: { compact?: boolean }) {
  const { data, retry } = useWorkspace();
  const pathname = usePathname();
  const router = useRouter();
  const [menu, setMenu] = useState<Selection | null>(null);
  const [action, setAction] = useState<(Selection & { kind: "rename" | "delete" }) | null>(null);
  const menuId = useId();
  const closeMenu = useCallback(() => setMenu(null), []);

  if (!data?.conversations.length) return <p className={styles.empty} data-compact={compact}>Your conversations will appear here.</p>;

  function closeAction() {
    setAction(null);
    action?.trigger.focus();
  }

  return <>
    {data.conversations.map(conversation => {
      const href = `/app/conversations/${encodeURIComponent(conversation.id)}`;
      return <div key={conversation.id} className={styles.row} data-compact={compact} data-active={pathname === href} data-open={menu?.conversation.id === conversation.id}>
        <Link className={styles.link} aria-current={pathname === href ? "page" : undefined} href={href} title={conversation.title}>
          <span>{conversation.title}</span>
          {["queued", "running"].includes(conversation.status) ? <small>Working…</small> : conversation.status === "failed" ? <small>Needs attention</small> : null}
        </Link>
        <button type="button" className={styles.more} aria-label={`Options for ${conversation.title}`} aria-haspopup="menu" aria-expanded={menu?.conversation.id === conversation.id} aria-controls={menu?.conversation.id === conversation.id ? menuId : undefined} onClick={event => setMenu(menu?.conversation.id === conversation.id ? null : { conversation, trigger: event.currentTarget })}>
          <WorkspaceIcon name="more" width="18" height="18" />
        </button>
      </div>;
    })}
    {menu ? <ConversationMenu key={menu.conversation.id} id={menuId} selection={menu} onClose={closeMenu} onSelect={kind => { setAction({ ...menu, kind }); setMenu(null); }} /> : null}
    {action ? <ConversationDialog key={`${action.conversation.id}:${action.kind}`} conversation={action.conversation} kind={action.kind} workspace={data.workspace.id} onClose={closeAction} onSaved={() => {
      if (action.kind === "delete" && pathname === `/app/conversations/${encodeURIComponent(action.conversation.id)}`) router.replace("/app");
      retry();
      closeAction();
    }} /> : null}
  </>;
}

function ConversationMenu({ id, selection, onClose, onSelect }: { id: string; selection: Selection; onClose: () => void; onSelect: (kind: "rename" | "delete") => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const rect = selection.trigger.getBoundingClientRect();
  const left = Math.max(8, Math.min(rect.right - 164, window.innerWidth - 172));
  const top = rect.bottom + 100 > window.innerHeight ? rect.top - 88 : rect.bottom + 4;
  useEffect(() => {
    ref.current?.querySelector("button")?.focus();
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !ref.current?.contains(event.target) && !selection.trigger.contains(event.target)) onClose(); };
    const reposition = () => onClose();
    document.addEventListener("pointerdown", outside);
    window.addEventListener("resize", reposition);
    window.addEventListener("scroll", reposition, true);
    return () => {
      document.removeEventListener("pointerdown", outside);
      window.removeEventListener("resize", reposition);
      window.removeEventListener("scroll", reposition, true);
    };
  }, [selection, onClose]);

  return createPortal(<div ref={ref} id={id} role="menu" aria-label="Conversation options" className={styles.menu} style={{ left, top }} onKeyDown={event => {
    if (event.key === "Escape") { event.preventDefault(); onClose(); selection.trigger.focus(); }
    if (event.key === "Tab") { onClose(); selection.trigger.focus(); }
    if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
      event.preventDefault();
      const buttons = Array.from(event.currentTarget.querySelectorAll("button"));
      const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
      buttons[event.key === "Home" ? 0 : event.key === "End" ? buttons.length - 1 : (index + (event.key === "ArrowDown" ? 1 : -1) + buttons.length) % buttons.length]?.focus();
    }
  }}>
    <button type="button" role="menuitem" onClick={() => onSelect("rename")}><WorkspaceIcon name="edit" width="15" height="15" />Rename</button>
    <button type="button" role="menuitem" className={styles.danger} onClick={() => onSelect("delete")}><WorkspaceIcon name="trash" width="15" height="15" />Delete</button>
  </div>, document.body);
}

function ConversationDialog({ conversation, kind, workspace, onClose, onSaved }: { conversation: Conversation; kind: "rename" | "delete"; workspace: string; onClose: () => void; onSaved: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const descriptionId = useId();
  const [title, setTitle] = useState(conversation.title);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    input.current?.select();
    return () => element?.close();
  }, []);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await canterFetch(`${conversationBase(workspace)}/${encodeURIComponent(conversation.id)}`, kind === "rename" ? { method: "PATCH", body: JSON.stringify({ title: title.trim() }) } : { method: "DELETE" });
      if (kind === "delete") {
        const draftKey = `canter:conversation-draft:${workspace}:${conversation.id}`;
        try { sessionStorage.removeItem(draftKey); } catch { /* Optional local storage. */ }
        clearOperatorAttachmentDraft(draftKey);
      }
      onSaved();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "The conversation could not be updated.");
      setBusy(false);
    }
  }

  return createPortal(<dialog ref={dialog} className={styles.dialog} aria-labelledby={titleId} aria-describedby={kind === "delete" ? descriptionId : undefined} onCancel={event => { event.preventDefault(); if (!busy) onClose(); }}>
    <form onSubmit={submit}>
      <h2 id={titleId}>{kind === "rename" ? "Rename conversation" : "Delete conversation?"}</h2>
      {kind === "rename" ? <label className={styles.field}>Title<input ref={input} value={title} maxLength={100} required disabled={busy} onChange={event => setTitle(event.target.value)} /></label> : <p id={descriptionId}>This will permanently delete “{conversation.title}” and its messages. Already submitted operations will continue.</p>}
      {error ? <p className={styles.error} role="alert">{error}</p> : null}
      <div className={styles.actions}>
        <button type="button" disabled={busy} onClick={onClose} autoFocus={kind === "delete"}>Cancel</button>
        <button type="submit" className={kind === "delete" ? styles.deleteButton : styles.saveButton} disabled={busy || (kind === "rename" && !title.trim())}>{busy ? kind === "rename" ? "Saving…" : "Deleting…" : kind === "rename" ? "Save" : "Delete"}</button>
      </div>
    </form>
  </dialog>, document.body);
}
