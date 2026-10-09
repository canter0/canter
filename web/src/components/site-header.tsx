"use client";

import Link from "next/link";
import { useCallback, useId, useRef, useState } from "react";
import { usePopover } from "./use-popover";
import { moveMenuFocus } from "@/lib/interaction";
import { ViewTransition } from "react";
import { MotionPresence } from "./motion-presence";
import styles from "./site-header.module.css";

function LinkArrow() {
  return (
    <svg className={styles.arrow} width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M6 18 18 6M6 6h12v12" />
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
    <ViewTransition name="canter-site-header" default="none" share="canter-anchor"><header className={styles.header}>
      <Link href="/" className={`wordmark ${styles.wordmark}`} aria-label="Canter home">canter</Link>
      <nav className={styles.navigation} aria-label="Main navigation">
        <Link href="/pricing" className={styles.secondaryLink}>Pricing</Link>
      </nav>
      <div ref={menu} className={styles.mobileMenu}>
        <button type="button" className={styles.mobileTrigger} aria-label="Site navigation" aria-haspopup="menu" aria-expanded={open} aria-controls={open ? menuId : undefined} onClick={() => setOpen(!open)}><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d={open ? "M6 6l12 12M6 18L18 6" : "M4 7h16M4 12h16M4 17h16"} /></svg></button>
        <MotionPresence open={open}><div id={menuId} className={styles.mobileLinks} role="menu" aria-label="Site navigation" onKeyDown={moveMenuFocus}><Link role="menuitem" tabIndex={-1} href="/pricing" onNavigate={close}>Pricing</Link><Link role="menuitem" tabIndex={-1} href="/sign-in" onNavigate={close}>Sign in</Link></div></MotionPresence>
      </div>
      <div className={styles.actions}>
        <Link href="/sign-in" className={styles.signIn}>Sign in</Link>
        <Link href="/create-account" className={styles.getStarted}>Get Started <LinkArrow /></Link>
      </div>
    </header></ViewTransition>
  );
}
