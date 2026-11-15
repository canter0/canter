"use client";

import Link from "next/link";
import { useCallback, useEffect, useId, useRef, useState, useSyncExternalStore, type CSSProperties, type RefObject } from "react";
import { createPortal } from "react-dom";
import { notificationReceiptKey, parseNotificationReceipts, securityNotifications, workspaceNotifications, type WorkspaceNotification } from "@/lib/workspace-notifications";
import { canterFetch, relativeTime } from "@/lib/canter-api";
import type { AuditEvent } from "@/lib/audit";
import { selectWorkspace, openSelectedWorkspace } from "@/lib/workspace-selection";
import { useWorkspace } from "./workspace-context";
import { useMediaQuery } from "./use-media-query";
import { useFocusContainment } from "./use-focus-containment";
import { MotionPresence } from "./motion-presence";
import { MotionNav } from "./motion-nav";
import { WorkspaceIcon } from "./workspace-icon";
import shell from "./workspace.module.css";
import styles from "./workspace-inbox.module.css";

function subscribeToReceipts(changed: () => void) {
  window.addEventListener("storage", changed);
  window.addEventListener("canter-notification-read", changed);
  return () => { window.removeEventListener("storage", changed); window.removeEventListener("canter-notification-read", changed); };
}
function noReceipts() { return ""; }

function InviteNotification({ item, onRead, onChanged, onAccepted }: { item: WorkspaceNotification; onRead: (ids: string[]) => void; onChanged: () => void; onAccepted: (item: WorkspaceNotification) => void }) {
  const [state, setState] = useState<"idle" | "busy" | "accepted">("idle");
  const [opening, setOpening] = useState(false);
  const [error, setError] = useState("");
  async function respond(action: "accept" | "decline") {
    if (state === "busy") return;
    setState("busy"); setError("");
    try {
      await canterFetch(`/me/invites/${encodeURIComponent(item.inviteId!)}/${action}`, { method: "POST" });
      if (action === "accept") { onAccepted(item); setState("accepted"); onChanged(); } else { onRead([item.id]); onChanged(); }
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t respond to this invitation."); setState("idle"); }
  }
  async function open() {
    if (!item.workspaceId || opening) return;
    setOpening(true); setError("");
    try { await selectWorkspace(item.workspaceId); openSelectedWorkspace(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "You joined, but the organization could not be opened."); setOpening(false); }
  }
  return <span className={styles.notification}>
    <span className={styles.itemIcon} data-kind="invite"><WorkspaceIcon name="profile" width="18" height="18" /></span>
    <span className={styles.itemBody}>
      <strong>{item.title}</strong>
      <span>{item.detail}</span>
      {item.updatedAt ? <time dateTime={item.updatedAt}>{relativeTime(item.updatedAt) === "now" ? "just now" : `${relativeTime(item.updatedAt)} ago`}</time> : null}
      {state === "accepted" ? <span className={styles.inviteActions}><button type="button" disabled={opening} onClick={() => void open()}>{opening ? "Opening…" : "Open organization"}</button></span> : <span className={styles.inviteActions}><button type="button" disabled={state === "busy"} onClick={() => void respond("accept")}>Accept</button><button type="button" disabled={state === "busy"} onClick={() => void respond("decline")}>Decline</button></span>}
      {error ? <span role="alert">{error}</span> : null}
    </span>
  </span>;
}

export function WorkspaceInbox({ collapsed, onOpen, navigationTrigger }: { collapsed: boolean; onOpen: () => void; navigationTrigger: RefObject<HTMLButtonElement | null> }) {
  const { data, resources, retry } = useWorkspace();
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState<"all" | "unread">("all");
  const [accepted, setAccepted] = useState<Record<string, WorkspaceNotification>>({});
  const [fallback, setFallback] = useState({ key: "", raw: "", localOnly: false });
  const trigger = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLElement>(null);
  const id = useId();
  const mobile = useMediaQuery("(max-width: 1023px)");
  const key = notificationReceiptKey(data.account.id, data.workspace.id);
  const getReceipts = useCallback(() => { if (fallback.key === key && fallback.localOnly) return fallback.raw; try { return localStorage.getItem(key) ?? ""; } catch { return fallback.key === key ? fallback.raw : ""; } }, [key, fallback]);
  const raw = useSyncExternalStore(subscribeToReceipts, getReceipts, noReceipts);
  const read = parseNotificationReceipts(raw);
  const [security, setSecurity] = useState<AuditEvent[]>([]);
  const securityAllowed = data.workspace.role === "owner" || data.workspace.role === "operator";
  useEffect(() => {
    if (!securityAllowed) return;
    const controller = new AbortController();
    canterFetch<{ events: AuditEvent[] }>(`/workspaces/${encodeURIComponent(data.workspace.id)}/audit?limit=50`, { signal: controller.signal }).then(page => setSecurity(page.events)).catch(() => {});
    return () => controller.abort();
  }, [data.workspace.id, securityAllowed]);
  const notifications = [...workspaceNotifications(data), ...securityNotifications(securityAllowed ? security : [], data.account.id)].sort((a, b) => (Date.parse(b.updatedAt) || 0) - (Date.parse(a.updatedAt) || 0));
  const unread = notifications.filter(item => !read[item.id]);
  // Accepted invites leave /me/invites immediately; keep them rendered so the
  // "Open organization" action survives the refresh.
  const base = filter === "unread" ? unread : notifications;
  const shown = [...base, ...Object.values(accepted).filter(item => !base.some(current => current.id === item.id))];
  const sources = [resources.conversations, resources.tasks, resources.changes, resources.initialDeployments, resources.invites];
  const failed = sources.some(source => source.error);
  const checking = sources.some(source => !source.loaded);
  const close = useCallback(() => setOpen(false), []);
  useFocusContainment(open && mobile, panel, close, navigationTrigger);

  useEffect(() => {
    if (!open || mobile) return;
    const element = panel.current;
    const previous = document.activeElement;
    element?.querySelector<HTMLButtonElement>("button:not(:disabled)")?.focus();
    const keyboard = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !event.defaultPrevented) { event.preventDefault(); close(); trigger.current?.focus(); }
    };
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !panel.current?.contains(event.target) && !trigger.current?.contains(event.target)) close();
    };
    document.addEventListener("keydown", keyboard);
    document.addEventListener("pointerdown", outside);
    return () => { document.removeEventListener("keydown", keyboard); document.removeEventListener("pointerdown", outside); if (element?.contains(document.activeElement) && previous instanceof HTMLElement && previous.isConnected) previous.focus({ preventScroll: true }); };
  }, [open, mobile, close]);

  function markRead(ids: string[]) {
    const next = { ...read };
    for (const id of ids) next[id] = true;
    const snapshot = JSON.stringify(Object.fromEntries(Object.entries(next).slice(-400)));
    let localOnly = false;
    try { localStorage.setItem(key, snapshot); window.dispatchEvent(new Event("canter-notification-read")); } catch { localOnly = true; }
    setFallback({ key, raw: snapshot, localOnly });
  }
  function dismiss() { close(); if (!mobile) trigger.current?.focus(); }

  return <>
    <button ref={trigger} type="button" className={`${shell.iconButton} ${shell.workspaceInbox} ${styles.trigger}`} aria-label={`Notifications${unread.length ? `, ${unread.length} unread` : ""}`} title="Notifications" aria-haspopup="dialog" aria-expanded={open} aria-controls={open ? id : undefined} onClick={() => { if (!open) onOpen(); setOpen(!open); }}><WorkspaceIcon name="inbox" />{unread.length ? <span className={styles.unreadDot} aria-hidden="true" /> : null}</button>
    {typeof document !== "undefined" ? createPortal(<MotionPresence open={open}><aside ref={panel} tabIndex={-1} role="dialog" aria-modal={mobile || undefined} aria-label="Notifications" id={id} className={`dashboard-theme ${styles.panel}`} style={{ "--inbox-left": `${56 + (collapsed ? 64 : 300)}px` } as CSSProperties}>
      <header className={styles.header}><h2>Notifications</h2><div className={styles.headerActions}><button type="button" disabled={!unread.length} aria-label="Mark all notifications as read" title="Mark all as read" onClick={() => markRead(unread.map(item => item.id))}><WorkspaceIcon name="check" width="17" height="17" /></button><button type="button" aria-label="Close notifications" onClick={dismiss}><WorkspaceIcon name="close" width="18" height="18" /></button></div></header>
      <MotionNav name="notification-tabs" label="Notification filters" role="tablist" className={styles.tabs}><button type="button" role="tab" aria-selected={filter === "all"} aria-controls={`${id}-items`} id={`${id}-all`} onClick={() => setFilter("all")} onKeyDown={event => { if (event.key === "ArrowRight") { event.preventDefault(); setFilter("unread"); panel.current?.querySelector<HTMLButtonElement>(`#${CSS.escape(id)}-unread`)?.focus(); } }}>All</button><button type="button" role="tab" aria-selected={filter === "unread"} aria-controls={`${id}-items`} id={`${id}-unread`} onClick={() => setFilter("unread")} onKeyDown={event => { if (event.key === "ArrowLeft") { event.preventDefault(); setFilter("all"); panel.current?.querySelector<HTMLButtonElement>(`#${CSS.escape(id)}-all`)?.focus(); } }}>Unread <span>{unread.length}</span></button></MotionNav>
      <div className={styles.content} role="tabpanel" id={`${id}-items`} aria-labelledby={`${id}-${filter}`}>
        {failed ? <div className={styles.syncNotice} role="status">Some notifications couldn’t refresh.<button type="button" onClick={retry}>Retry</button></div> : null}
        {shown.length ? <ul className={styles.list}>{shown.map(item => <li key={item.id} data-unread={!read[item.id]}>{item.kind === "invite" && item.inviteId ? <InviteNotification item={item} onRead={markRead} onChanged={retry} onAccepted={item => setAccepted(current => ({ ...current, [item.id]: item }))} /> : <><Link href={item.href} onClick={() => markRead([item.id])} onNavigate={close} className={styles.notification}><span className={styles.itemIcon} data-kind={item.kind}><WorkspaceIcon name={item.kind === "review" ? "changes" : item.kind === "security" ? "lock" : "activity"} width="18" height="18" /></span><span className={styles.itemBody}><strong>{item.title}</strong><span>{item.detail}</span>{item.updatedAt ? <time dateTime={item.updatedAt}>{relativeTime(item.updatedAt) === "now" ? "just now" : `${relativeTime(item.updatedAt)} ago`}</time> : null}</span>{!read[item.id] ? <span className={styles.itemDot} aria-label="Unread" /> : null}</Link>{!read[item.id] ? <button type="button" className={styles.markRead} aria-label={`Mark ${item.title} as read`} title="Mark as read" onClick={() => markRead([item.id])}><WorkspaceIcon name="check" width="14" height="14" /></button> : null}</>}</li>)}</ul> : <div className={styles.empty} role="status"><span className={styles.emptyIcon}><WorkspaceIcon name="bell" width="24" height="24" /></span><h3>{checking ? "Checking your organization…" : failed ? "Couldn’t load notifications" : "You’re all caught up"}</h3><p>{checking ? "Looking for updates that need your attention." : failed ? "Retry to check for updates." : filter === "unread" ? "You have no unread notifications." : "New notifications will show up here."}</p></div>}
      </div>
    </aside></MotionPresence>, document.body) : null}
  </>;
}
