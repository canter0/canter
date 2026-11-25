"use client";

import Link from "next/link";
import { useEffect, useRef, ViewTransition } from "react";
import { SiteProductMenu } from "./site-product-menu";
import styles from "./site-header.module.css";

const bookingURL = "https://cal.com/y9090";

function LinkArrow() {
  return (
    <svg className={styles.arrow} width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M6 18 18 6M6 6h12v12" />
    </svg>
  );
}

export function SiteHeader({ sticky = false, getStartedHref }: { sticky?: boolean; getStartedHref?: string }) {
  const header = useRef<HTMLElement>(null);
  const getStartedLabel = <>Get Started <LinkArrow /></>;

  useEffect(() => {
    if (!sticky || !header.current) return;
    const element = header.current;
    function update() {
      element.toggleAttribute("data-floating", window.scrollY > 8);
    }
    update();
    window.addEventListener("scroll", update, { passive: true });
    return () => window.removeEventListener("scroll", update);
  }, [sticky]);

  return (
    <ViewTransition name="canter-site-header" default="none" share="canter-anchor"><header ref={header} className={sticky ? `${styles.header} ${styles.sticky}` : styles.header}>
      <Link href="/" className={`wordmark ${styles.wordmark}`} aria-label="Canter home">canter</Link>
      <SiteProductMenu bookingURL={bookingURL} />
      <div className={styles.actions}>
        <Link href="/sign-in" className={styles.signIn}>Sign in</Link>
        <a href={bookingURL} className={styles.bookCall}>Book a call</a>
        {getStartedHref
          ? <a href={getStartedHref} className={styles.getStarted}>{getStartedLabel}</a>
          : <Link href="/create-account" className={styles.getStarted}>{getStartedLabel}</Link>}
      </div>
    </header></ViewTransition>
  );
}
