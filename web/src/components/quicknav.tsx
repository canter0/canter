"use client";

import { useEffect, useRef, useState, type ButtonHTMLAttributes, type FocusEvent, type ReactNode } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { sessionUpdatedAt } from "./conversation-list";
import { MotionPresence } from "./motion-presence";
import { WorkspaceIcon } from "./workspace-icon";
import { WrenMark } from "./wren-mark";
import { useWorkspace } from "./workspace-context";
import { quicknavFooterItems, quicknavItems, type QuicknavItem } from "../lib/quicknav-items";
import styles from "./quicknav.module.css";

const hoverDelay = 50;
const homeExcluded = ["/app/wrens", "/app/settings", "/app/account", "/app/billing", "/app/sandboxes", "/app/products", "/app/customize", "/app/ci"];

export function Quicknav({ className = "", inert, onNavigate, account }: { className?: string; inert?: boolean; onNavigate?: () => void; account?: ReactNode }) {
  const pathname = usePathname();
  const { data } = useWorkspace();
  const items = quicknavItems(data?.products);
  const accountName = data?.account.email.split("@")[0] || "Account";
  return <nav className={`${styles.rail} ${className}`} aria-label="Quicknav" inert={inert}>
    <div className={styles.group}>{items.map(item => <RailItem key={item.href} item={item} pathname={pathname} onNavigate={onNavigate} />)}</div>
    <div className={`${styles.group} ${styles.footer}`}>{quicknavFooterItems.map(item => <RailItem key={item.href} item={item} pathname={pathname} onNavigate={onNavigate} />)}
      {account ?? <Link href="/app/account" className={styles.profile} aria-label={`Profile for ${accountName}`} title="Profile" aria-current={pathname.startsWith("/app/account") ? "location" : undefined} onNavigate={onNavigate}><ProfileAvatar name={accountName} /></Link>}
    </div>
  </nav>;
}

function ProfileAvatar({ name }: { name: string }) {
  return <span className={styles.avatar} aria-hidden="true">{name.slice(0, 1).toUpperCase()}</span>;
}

export function ProfileButton({ name, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { name: string }) {
  return <button type="button" className={styles.profile} {...props}><ProfileAvatar name={name} /></button>;
}

function RailItem({ item, pathname, onNavigate }: { item: QuicknavItem; pathname: string; onNavigate?: () => void }) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const timer = useRef(0);
  const current = item.href === "/app" ? !homeExcluded.some(path => pathname.startsWith(path)) : pathname.startsWith(item.href);
  const schedule = (next: boolean) => { window.clearTimeout(timer.current); timer.current = window.setTimeout(() => setOpen(next), hoverDelay); };
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const navigate = () => { setOpen(false); onNavigate?.(); };
  function blur(event: FocusEvent<HTMLDivElement>) {
    if (!(event.relatedTarget instanceof Node && root.current?.contains(event.relatedTarget))) setOpen(false);
  }
  return <div ref={root} className={styles.item} onPointerEnter={event => { if (event.pointerType === "mouse") schedule(true); }} onPointerLeave={event => { if (event.pointerType === "mouse") schedule(false); }} onFocus={event => { if (event.target instanceof Element && event.target.matches(":focus-visible")) setOpen(true); }} onBlur={blur} onKeyDown={event => { if (event.key === "Escape") setOpen(false); }}>
    <Link href={item.href} prefetch={item.href === "/app"} aria-label={item.label} title={item.preview ? undefined : item.label} aria-current={current ? "location" : undefined} className={styles.link} onNavigate={navigate}>{item.wren ? <WrenMark /> : <WorkspaceIcon name={item.icon ?? "apps"} width="20" height="20" />}</Link>
    {item.preview ? <MotionPresence open={open}><div className={styles.card}><RailPreview item={item} onNavigate={navigate} /></div></MotionPresence> : null}
  </div>;
}

function RailPreview({ item, onNavigate }: { item: QuicknavItem; onNavigate: () => void }) {
  const { data } = useWorkspace();
  if (item.preview === "recent") {
    const recent = (data?.conversations ?? []).slice(0, 3);
    return <>
      <Link className={styles.cardAction} href="/app?compose=1" onNavigate={onNavigate}><WorkspaceIcon name="plus" width="16" height="16" />New conversation</Link>
      <div className={styles.cardSeparator} />
      <p className={styles.cardLabel}>Recent</p>
      {recent.length ? recent.map(conversation => <Link key={conversation.id} className={styles.cardRow} href={`/app/conversations/${encodeURIComponent(conversation.id)}`} onNavigate={onNavigate}><span>{conversation.title || "Untitled conversation"}</span><small>{sessionUpdatedAt(conversation.updatedAt)}</small></Link>) : <p className={styles.cardEmpty}>No conversations yet</p>}
    </>;
  }
  return <>
    <p className={styles.cardTitle}>{item.label}</p>
    {item.description ? <p className={styles.cardText}>{item.description}</p> : null}
    <Link className={styles.cardAction} href={item.href} onNavigate={onNavigate}>Open {item.label}<WorkspaceIcon name="right" width="14" height="14" /></Link>
  </>;
}
