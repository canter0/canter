"use client";

import { useLayoutEffect, useRef, type ReactNode } from "react";
import { motion, playMotion } from "@/lib/motion";

type Position = { left: number; top: number; width: number; height: number };
const positions = new Map<string, Position>();

/** The active background travels between links, including across route remounts. */
export function MotionNav({ name, label, className, role, children }: { name: string; label: string; className?: string; role?: "group" | "tablist"; children: ReactNode }) {
  const root = useRef<HTMLElement>(null);
  const indicator = useRef<HTMLSpanElement>(null);
  const update = useRef<() => void>(() => {});
  useLayoutEffect(() => {
    const nav = root.current;
    const highlight = indicator.current;
    if (!nav || !highlight) return;
    let animation: Animation | null = null;
    let frame = 0;
    const position = () => {
      const link = nav.querySelector<HTMLElement>('[aria-current="page"]:not([data-motion-nav-skip]), [aria-pressed="true"], [aria-selected="true"]');
      if (!link) { highlight.hidden = true; return; }
      const bounds = nav.getBoundingClientRect();
      const rect = link.getBoundingClientRect();
      const next = { left: rect.left - bounds.left + nav.scrollLeft, top: rect.top - bounds.top + nav.scrollTop, width: rect.width, height: rect.height };
      const moving = animation?.playState === "running" ? highlight.getBoundingClientRect() : null;
      const previous = moving ? {
        left: moving.left - bounds.left + nav.scrollLeft,
        top: moving.top - bounds.top + nav.scrollTop,
        width: moving.width,
        height: moving.height,
      } : positions.get(name);
      const target = positions.get(name);
      positions.set(name, next);
      highlight.hidden = false;
      Object.assign(highlight.style, { left: `${next.left}px`, top: `${next.top}px`, width: `${next.width}px`, height: `${next.height}px` });
      nav.dataset.motionNavReady = "true";
      if (!previous || (target?.left === next.left && target.top === next.top && target.width === next.width && target.height === next.height)) return;
      animation?.cancel();
      animation = playMotion(highlight, [
        { transform: `translate(${previous.left - next.left}px, ${previous.top - next.top}px)`, width: `${previous.width}px`, height: `${previous.height}px` },
        { transform: "none", width: `${next.width}px`, height: `${next.height}px` },
      ], { duration: motion.layout });
    };
    const schedule = () => { cancelAnimationFrame(frame); frame = requestAnimationFrame(position); };
    update.current = position;
    position();
    const resize = new ResizeObserver(schedule);
    resize.observe(nav);
    const observer = new MutationObserver(schedule);
    observer.observe(nav, { attributes: true, attributeFilter: ["aria-current", "aria-pressed", "aria-selected"], childList: true, subtree: true });
    return () => { observer.disconnect(); resize.disconnect(); cancelAnimationFrame(frame); animation?.cancel(); };
  }, [name]);
  useLayoutEffect(() => { update.current(); }, [children]);
  return <nav ref={root} className={className} role={role} aria-label={label} data-motion-nav={name}>
    <span ref={indicator} className="motion-nav-indicator" aria-hidden="true" hidden />{children}
  </nav>;
}
