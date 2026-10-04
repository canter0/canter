"use client";

import Link from "next/link";
import { useCallback, useId, useRef, useState } from "react";
import { usePopover } from "./use-popover";
import { moveMenuFocus } from "@/lib/interaction";
import styles from "./site-header.module.css";

function LinkArrow() {
  return (
    <svg className={styles.arrow} width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M7 17 17 7M7 7h10v10" />
    </svg>
  );
}

export function SiteHeader() {
  const [open, setOpen] = useState(false);
  const menu = useRef<HTMLDivElement>(null);
  const menuId = useId();
  const close = useCallback(() => setOpen(false), []);
  usePopover(open, menu, close);
  return (
    <header className={styles.header}>
      <Link href="/" className={`wordmark ${styles.wordmark}`} aria-label="Canter home">canter</Link>
      <nav className={styles.navigation} aria-label="Main navigation">
        <Link href="/pricing" className={styles.secondaryLink}>Pricing</Link>
        <a href="https://github.com/canter0/canter#how-it-works" className={styles.secondaryLink}>Docs</a>
        <a href="https://github.com/canter0/canter" className={styles.secondaryLink}>GitHub <LinkArrow /></a>
      </nav>
      <div ref={menu} className={styles.mobileMenu}>
        <button type="button" className={styles.mobileTrigger} aria-label="Site navigation" aria-haspopup="menu" aria-expanded={open} aria-controls={open ? menuId : undefined} onClick={() => setOpen(!open)}><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d={open ? "M6 6l12 12M6 18L18 6" : "M4 7h16M4 12h16M4 17h16"} /></svg></button>
        {open ? <div id={menuId} className={styles.mobileLinks} role="menu" aria-label="Site navigation" onKeyDown={moveMenuFocus}><Link role="menuitem" tabIndex={-1} href="/pricing" onNavigate={close}>Pricing</Link><a role="menuitem" tabIndex={-1} href="https://github.com/canter0/canter#how-it-works">Docs</a><a role="menuitem" tabIndex={-1} href="https://github.com/canter0/canter">GitHub <LinkArrow /></a><Link role="menuitem" tabIndex={-1} href="/sign-in" onNavigate={close}>Sign in</Link></div> : null}
      </div>
      <div className={styles.actions}>
        <Link href="/sign-in" className={styles.signIn}>Sign in</Link>
        <Link href="/create-account" className={styles.getStarted}>Get started <LinkArrow /></Link>
      </div>
    </header>
  );
}
