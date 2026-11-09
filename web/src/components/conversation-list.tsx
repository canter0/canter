"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useEffect, useId, useRef, useState, type FormEvent } from "react";
import { createPortal } from "react-dom";
import { canterFetch, relativeTime } from "@/lib/canter-api";
import { conversationBase, type Conversation } from "@/lib/operator-api";
import { operatorDraftKey, operatorReplyDraftKey } from "@/lib/operator-draft-storage";
import { clearOperatorAttachmentDraft } from "@/lib/operator-attachment-draft";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import { useDialog } from "./use-dialog";
import { MorphLabel } from "./conversation-motion";
import styles from "./conversation-list.module.css";

type Selection = { conversation: Conversation; trigger: HTMLButtonElement; isOwner: boolean };
type ActionKind = "rename" | "delete" | "share" | "unshare";

function sessionUpdatedAt(value: string) {
  const time = relativeTime(value);
  if (time === "now") return "just now";
  if (time === "1 days") return "yesterday";
  return time.replace(/^(\d+) min$/, "$1 minutes ago").replace(/^(\d+) hr$/, "$1 hours ago").replace(/^(\d+) days$/, "$1 days ago").replace(/^1 minutes/, "1 minute").replace(/^1 hours/, "1 hour");
}

export function ConversationList({ compact = false, order = "recent" }: { compact?: boolean; order?: "recent" | "oldest" }) {
  const { data, retry, conversationCache, prefetchConversation } = useWorkspace();
  const pathname = usePathname();
  const router = useRouter();
  const [menu, setMenu] = useState<Selection | null>(null);
  const [action, setAction] = useState<(Selection & { kind: ActionKind }) | null>(null);
  const menuId = useId();
  const closeMenu = useCallback(() => setMenu(null), []);

  if (!data?.conversations.length) return <p className={styles.empty} data-compact={compact}>Your conversations will appear here.</p>;

  function closeAction() {
    const trigger = action?.trigger;
    setAction(null);
    requestAnimationFrame(() => { if (trigger?.isConnected) trigger.focus(); });
  }

  return <>
    {(order === "oldest" ? [...data.conversations].sort((a, b) => Date.parse(a.updatedAt) - Date.parse(b.updatedAt)) : data.conversations).map(conversation => {
      const href = `/app/conversations/${encodeURIComponent(conversation.id)}`;
      return <div key={conversation.id} className={styles.row} data-compact={compact} data-active={pathname === href} data-open={menu?.conversation.id === conversation.id}>
        <span className={styles.sessionLeading} aria-hidden="true"><span /></span>
        <Link className={styles.link} aria-label={conversation.title} aria-current={pathname === href ? "page" : undefined} href={href} prefetch={false} onPointerEnter={() => prefetchConversation(conversation.id)} onFocus={() => prefetchConversation(conversation.id)} onTouchStart={() => prefetchConversation(conversation.id)} title={conversation.title}>
          <span>{conversation.title}</span>
          <small className={styles.sessionMetadata}><span className={styles.sessionAgent} aria-hidden="true" />{conversation.ownerAccountId && conversation.ownerAccountId !== data.account.id ? <span className={styles.sharedMarker}>{conversation.ownerEmail || "Shared"} · </span> : conversation.shared ? <span className={styles.sharedMarker}>Shared · </span> : null}{["queued", "running"].includes(conversation.status) ? "Working…" : conversation.status === "failed" ? "Needs attention" : <time dateTime={conversation.updatedAt}>{sessionUpdatedAt(conversation.updatedAt)}</time>}</small>
        </Link>
        <button type="button" className={styles.more} aria-label={`Options for ${conversation.title}`} aria-haspopup="menu" aria-expanded={menu?.conversation.id === conversation.id} aria-controls={menu?.conversation.id === conversation.id ? menuId : undefined} onClick={event => setMenu(menu?.conversation.id === conversation.id ? null : { conversation, trigger: event.currentTarget, isOwner: !conversation.ownerAccountId || conversation.ownerAccountId === data?.account.id })}>
          <WorkspaceIcon name="more" width="18" height="18" />
        </button>
      </div>;
    })}
    {menu ? <ConversationMenu key={menu.conversation.id} id={menuId} selection={menu} onClose={closeMenu} onSelect={kind => { setAction({ ...menu, kind }); setMenu(null); }} /> : null}
    {action ? <ConversationDialog key={`${action.conversation.id}:${action.kind}`} conversation={action.conversation} kind={action.kind} workspace={data.workspace.id} account={data.account.id} onClose={closeAction} onSaved={() => {
      conversationCache.invalidate(action.conversation.id);
      if (action.kind === "delete" && pathname === `/app/conversations/${encodeURIComponent(action.conversation.id)}`) router.replace("/app");
      router.refresh();
      retry();
      closeAction();
    }} /> : null}
  </>;
}

function ConversationMenu({ id, selection, onClose, onSelect }: { id: string; selection: Selection; onClose: () => void; onSelect: (kind: ActionKind) => void }) {
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
    {selection.isOwner ? <button type="button" role="menuitem" onClick={() => onSelect(selection.conversation.shared ? "unshare" : "share")}><WorkspaceIcon name="profile" width="15" height="15" />{selection.conversation.shared ? "Stop sharing" : "Share with organization"}</button> : null}
    <button type="button" role="menuitem" className={styles.danger} onClick={() => onSelect("delete")}><WorkspaceIcon name="trash" width="15" height="15" />Delete</button>
  </div>, document.body);
}

function ConversationDialog({ conversation, kind, workspace, account, onClose, onSaved }: { conversation: Conversation; kind: ActionKind; workspace: string; account: string; onClose: () => void; onSaved: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const titleId = useId();
  const descriptionId = useId();
  const [title, setTitle] = useState(conversation.title);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const dismiss = useDialog(dialog);
  const close = () => dismiss(onClose);
  useEffect(() => {
    input.current?.select();
  }, []);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await canterFetch(`${conversationBase(workspace)}/${encodeURIComponent(conversation.id)}${kind === "share" || kind === "unshare" ? "/share" : ""}`, kind === "rename" ? { method: "PATCH", body: JSON.stringify({ title: title.trim() }) } : { method: kind === "share" ? "POST" : "DELETE" });
      if (kind === "delete") {
        const draftKey = operatorDraftKey(account, workspace, conversation.id);
        if (draftKey) {
          try { sessionStorage.removeItem(draftKey); const replyKey=operatorReplyDraftKey(account,workspace,conversation.id);if(replyKey)sessionStorage.removeItem(replyKey); } catch { /* Optional local storage. */ }
          clearOperatorAttachmentDraft(draftKey);
        }
      }
      onSaved();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "The conversation could not be updated.");
      setBusy(false);
    }
  }

  return createPortal(<dialog ref={dialog} className={styles.dialog} aria-labelledby={titleId} aria-describedby={kind !== "rename" ? descriptionId : undefined} onCancel={event => { event.preventDefault(); if (!busy) close(); }}>
    <form onSubmit={submit}>
      <h2 id={titleId}>{kind === "rename" ? "Rename conversation" : kind === "share" ? "Share with organization?" : kind === "unshare" ? "Stop sharing?" : "Delete conversation?"}</h2>
      {kind === "rename" ? <label className={styles.field}>Title<input ref={input} value={title} maxLength={100} required disabled={busy} onChange={event => setTitle(event.target.value)} /></label> : kind === "share" ? <p id={descriptionId}>Everyone in this organization will see the full history, attachments, and agent output of “{conversation.title}”.</p> : kind === "unshare" ? <p id={descriptionId}>Teammates will immediately lose access to “{conversation.title}”.</p> : <p id={descriptionId}>This will permanently delete “{conversation.title}” and its messages. Already submitted operations will continue.</p>}
      {error ? <p className={styles.error} role="alert">{error}</p> : null}
      <div className={styles.actions}>
        <button type="button" disabled={busy} onClick={close} autoFocus={kind !== "rename"}>Cancel</button>
        <button type="submit" className={kind === "delete" ? styles.deleteButton : styles.saveButton} disabled={busy || (kind === "rename" && !title.trim())}><MorphLabel text={busy ? kind === "rename" ? "Saving…" : kind === "delete" ? "Deleting…" : "Sharing…" : kind === "rename" ? "Save" : kind === "share" ? "Share" : kind === "unshare" ? "Stop sharing" : "Delete"} shimmer={busy} /></button>
      </div>
    </form>
  </dialog>, document.body);
}
