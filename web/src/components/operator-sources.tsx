"use client";

/* eslint-disable @next/next/no-img-element -- Favicons are small, cached remote images; avoid routing arbitrary source hosts through the image optimizer. */
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { WebSource } from "@/lib/operator-web-sources";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./operator-sources.module.css";

function SourceIcon({ url }: { url: string }) {
  const [failed, setFailed] = useState(false);
  const hostname = new URL(url).hostname;
  return <span className={styles.favicon} aria-hidden="true">
    {failed ? <WorkspaceIcon name="globe" width="16" height="16" /> : <img src={`https://www.google.com/s2/favicons?domain=${encodeURIComponent(hostname)}&sz=64`} width="16" height="16" alt="" loading="lazy" decoding="async" referrerPolicy="no-referrer" onError={() => setFailed(true)} />}
  </span>;
}

export function OperatorSources({ sources }: { sources: WebSource[] }) {
  const [active, setActive] = useState<WebSource | null>(null);
  const anchor = useRef<HTMLAnchorElement | null>(null);
  const preview = useRef<HTMLAnchorElement | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const id = useId();
  const cancelClose = useCallback(() => {
    if (timer.current !== null) clearTimeout(timer.current);
    timer.current = null;
  }, []);
  const close = useCallback(() => { cancelClose(); setActive(null); }, [cancelClose]);
  const deferClose = () => {
    cancelClose();
    timer.current = setTimeout(() => {
      // Pointer exit must not dismiss a preview that keyboard focus owns.
      if (document.activeElement !== anchor.current && !preview.current?.contains(document.activeElement)) setActive(null);
      timer.current = null;
    }, 120);
  };
  const show = (source: WebSource, element: HTMLAnchorElement) => { cancelClose(); anchor.current = element; setActive(source); };

  useLayoutEffect(() => {
    if (!active || !anchor.current || !preview.current) return;
    const place = () => {
      const link = anchor.current;
      const card = preview.current;
      if (!link || !card) return;
      const rect = link.getBoundingClientRect();
      const viewport = window.visualViewport;
      const left = viewport?.offsetLeft ?? 0;
      const top = viewport?.offsetTop ?? 0;
      const width = viewport?.width ?? window.innerWidth;
      const height = viewport?.height ?? window.innerHeight;
      const cardRect = card.getBoundingClientRect();
      const above = rect.top - cardRect.height - 9;
      const below = rect.bottom + 9;
      card.style.left = `${Math.max(left + 12, Math.min(rect.left, left + width - cardRect.width - 12))}px`;
      card.style.top = `${Math.max(top + 12, Math.min(above >= top + 12 ? above : below, top + height - cardRect.height - 12))}px`;
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    window.visualViewport?.addEventListener("resize", place);
    window.visualViewport?.addEventListener("scroll", place);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
      window.visualViewport?.removeEventListener("resize", place);
      window.visualViewport?.removeEventListener("scroll", place);
    };
  }, [active]);

  useEffect(() => {
    if (!active) return;
    const keyboard = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); close(); }
      else if (["PageUp", "PageDown", "Home", "End", "ArrowUp", "ArrowDown"].includes(event.key)) close();
    };
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !preview.current?.contains(event.target) && !anchor.current?.contains(event.target)) close();
    };
    const scroll = (event: Event) => {
      // Focusing an offscreen source scrolls it into view; retain that preview.
      if (document.activeElement !== anchor.current && !(event.target instanceof Node && preview.current?.contains(event.target))) close();
    };
    document.addEventListener("keydown", keyboard);
    document.addEventListener("pointerdown", outside);
    window.addEventListener("scroll", scroll, true);
    window.addEventListener("wheel", close, { passive: true });
    window.addEventListener("touchmove", close, { passive: true });
    return () => {
      document.removeEventListener("keydown", keyboard);
      document.removeEventListener("pointerdown", outside);
      window.removeEventListener("scroll", scroll, true);
      window.removeEventListener("wheel", close);
      window.removeEventListener("touchmove", close);
    };
  }, [active, close]);
  useEffect(() => cancelClose, [cancelClose]);

  return <div className={styles.sources} role="group" aria-label="Sources">
    {sources.map(source => <a key={source.url} className={styles.source} data-source-icon href={source.url} target="_blank" rel="noopener noreferrer" aria-label={`Source: ${source.title} (${new URL(source.url).hostname})`} aria-describedby={active?.url === source.url ? id : undefined}
      onPointerEnter={event => { if (event.pointerType !== "touch") show(source, event.currentTarget); }} onPointerLeave={deferClose}
      onFocus={event => show(source, event.currentTarget)} onBlur={event => { if (!preview.current?.contains(event.relatedTarget)) close(); }} onClick={close}>
      <SourceIcon url={source.url} />
    </a>)}
    {active ? createPortal(<a ref={preview} id={id} className={`dashboard-theme ${styles.preview}`} data-source-preview href={active.url} target="_blank" rel="noopener noreferrer" tabIndex={-1} onPointerEnter={cancelClose} onPointerLeave={deferClose} onBlur={close} onClick={close}>
      <span className={styles.previewSite}><SourceIcon key={active.url} url={active.url} /><span>{new URL(active.url).hostname.replace(/^www\./, "")}</span><WorkspaceIcon name="external" width="13" height="13" /></span>
      <strong>{active.title}</strong>
      <span className={styles.url}>{active.url}</span>
    </a>, document.body) : null}
  </div>;
}
