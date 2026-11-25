"use client";

import Link from "next/link";
import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { publicProducts, type PublicProductId } from "@/lib/public-products";
import { MotionPresence } from "./motion-presence";
import { useHoverPreview } from "./use-hover-preview";
import { useMediaQuery } from "./use-media-query";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import styles from "./site-product-menu.module.css";

const products: { id: string; label: string; icon: WorkspaceIconName; links: PublicProductId[] }[] = [
  { id: "sandboxes", label: "Sandboxes", icon: "sandboxTerminal", links: ["computers", "super-sandboxes", "saved-environments"] },
  { id: "vps", label: "VPS", icon: "serverStack", links: ["vps", "apps"] },
  { id: "ci", label: "CI", icon: "workflowCheck", links: ["ci", "ci-workspace"] },
];

export function SiteProductMenu({ bookingURL }: { bookingURL: string }) {
  const [open, setOpen] = useState<"hover" | "manual" | null>(null);
  const id = useId();
  const mobile = useMediaQuery("(max-width: 680px)");
  const root = useRef<HTMLElement>(null);
  const desktopTrigger = useRef<HTMLButtonElement>(null);
  const mobileTrigger = useRef<HTMLButtonElement>(null);
  const choices = useRef<HTMLDivElement>(null);
  const detail = useRef<HTMLDivElement>(null);
  const closeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [preview, select] = useHoverPreview(Boolean(open), mobile, choices, detail, {
    optionAttribute: "data-product-option",
    retainOnLeave: true,
  });
  const selected = products.find(product => product.id === preview) ?? products[0];

  const cancelClose = useCallback(() => {
    if (closeTimer.current !== null) clearTimeout(closeTimer.current);
    closeTimer.current = null;
  }, []);
  const close = useCallback(() => {
    cancelClose();
    setOpen(null);
  }, [cancelClose]);

  useEffect(() => {
    if (!open) return;
    function outside(event: PointerEvent) {
      if (event.target instanceof Node && !root.current?.contains(event.target)) close();
    }
    function escape(event: globalThis.KeyboardEvent) {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      if (root.current?.contains(document.activeElement)) {
        event.preventDefault();
        (mobile ? mobileTrigger : desktopTrigger).current?.focus();
      }
      close();
    }
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("keydown", escape);
    return () => {
      cancelClose();
      document.removeEventListener("pointerdown", outside, true);
      document.removeEventListener("keydown", escape);
    };
  }, [open, mobile, close, cancelClose]);

  function openMenu(mode: "hover" | "manual") {
    cancelClose();
    if (!open) select(products[0].id);
    setOpen(mode);
  }

  function toggle() {
    if (open === "manual") close();
    else openMenu("manual");
  }

  function moveTab(event: KeyboardEvent<HTMLDivElement>) {
    const nextKey = mobile ? "ArrowRight" : "ArrowDown";
    const previousKey = mobile ? "ArrowLeft" : "ArrowUp";
    if (![nextKey, previousKey, "Home", "End"].includes(event.key)) return;
    const tabs = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    const current = tabs.findIndex(tab => tab === event.target);
    if (current < 0) return;
    event.preventDefault();
    const next = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1
      : (current + (event.key === nextKey ? 1 : -1) + tabs.length) % tabs.length;
    tabs[next]?.focus();
  }

  return (
    <nav
      ref={root}
      className={styles.navigation}
      aria-label="Main navigation"
      onPointerEnter={cancelClose}
      onPointerLeave={event => {
        if (event.pointerType === "mouse" && open === "hover" && !root.current?.contains(document.activeElement)) {
          cancelClose();
          closeTimer.current = setTimeout(close, 160);
        }
      }}
      onFocusCapture={cancelClose}
      onBlur={event => {
        if (!event.currentTarget.contains(event.relatedTarget)) close();
      }}
    >
      <button ref={desktopTrigger} type="button" className={styles.trigger} aria-expanded={Boolean(open)} aria-controls={open ? id : undefined} onClick={toggle}
        onPointerEnter={event => { if (event.pointerType === "mouse" && !open) openMenu("hover"); }}>
        Product <WorkspaceIcon className={styles.caret} name="down" width={14} height={14} />
      </button>
      <button ref={mobileTrigger} type="button" className={styles.mobileTrigger} aria-label="Site navigation" aria-expanded={Boolean(open)} aria-controls={open ? id : undefined} onClick={toggle}>
        {open ? <WorkspaceIcon name="close" width={20} height={20} /> : <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16" /></svg>}
      </button>
      <MotionPresence open={Boolean(open)}>
        <div id={id} className={styles.panel}>
          <div ref={choices} className={styles.choices} role="tablist" aria-label="Products" aria-orientation={mobile ? "horizontal" : "vertical"} onKeyDown={moveTab}>
            {products.map(product => (
              <button key={product.id} className={styles.choice} type="button" role="tab" id={`${id}-${product.id}`} aria-selected={selected.id === product.id}
                aria-controls={selected.id === product.id ? `${id}-detail` : undefined} tabIndex={selected.id === product.id ? 0 : -1}
                data-product-option={product.id} onFocus={() => select(product.id)} onClick={() => select(product.id)}>
                <WorkspaceIcon className={styles.productIcon} name={product.icon} />
                <span>{product.label}</span>
              </button>
            ))}
          </div>
          <div ref={detail} className={styles.details} role="tabpanel" id={`${id}-detail`} aria-labelledby={`${id}-${selected.id}`}>
            <ul className={styles.detailRows}>
              {selected.links.map((product, index) => {
                const link = publicProducts[product];
                return <li key={link.path}>
                  <Link className={styles.detailLink} href={link.path} prefetch={false} onNavigate={close}
                    aria-label={link.label} aria-describedby={`${id}-${selected.id}-description-${index}`}>
                    <WorkspaceIcon className={styles.detailIcon} name={link.icon} />
                    <span className={styles.detailCopy}>
                      <span>{link.label}</span>
                      <span className={styles.description} id={`${id}-${selected.id}-description-${index}`}>{link.menuDescription}</span>
                    </span>
                  </Link>
                </li>;
              })}
            </ul>
          </div>
          <div className={styles.mobileLinks}>
            <Link href="/pricing" onNavigate={close}>Pricing</Link>
            <Link href="/sign-in" onNavigate={close}>Sign in</Link>
            <a href={bookingURL} className={styles.mobileBooking} onClick={close}>Book a call</a>
          </div>
        </div>
      </MotionPresence>
      <Link className={styles.pricing} href="/pricing" onNavigate={close} onPointerEnter={event => { if (event.pointerType === "mouse" && open === "hover" && !root.current?.contains(document.activeElement)) close(); }}>Pricing</Link>
    </nav>
  );
}
