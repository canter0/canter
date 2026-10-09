"use client";

import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { motion, playMotion } from "@/lib/motion";

/** Keep a dismissed popup for its exit, immediately removing it from interaction. */
export function MotionPresence({ open, children }: { open: boolean; children: ReactNode }) {
  const [present, setPresent] = useState(open);
  const root = useRef<HTMLDivElement>(null);
  const animation = useRef<Animation | null>(null);
  const generation = useRef(0);
  if (open && !present) setPresent(true);

  useLayoutEffect(() => {
    const element = root.current?.firstElementChild;
    if (!(element instanceof HTMLElement)) return;
    const token = ++generation.current;
    const previous = animation.current;
    const current = getComputedStyle(element);
    const from = previous ? { opacity: current.opacity, filter: current.filter, transform: current.transform } : { opacity: open ? 0 : 1, filter: open ? "blur(3px)" : "none", transform: open ? "translateY(4px) scale(.985)" : "none" };
    previous?.cancel();
    const next = playMotion(element, [from, open
      ? { opacity: 1, filter: "none", transform: "none" }
      : { opacity: 0, filter: "blur(3px)", transform: "translateY(3px) scale(.985)" }], { duration: open ? motion.panel : motion.exit });
    animation.current = next;
    const finish = () => {
      if (token !== generation.current) return;
      animation.current = null;
      if (!open) setPresent(false);
    };
    if (next) next.finished.then(finish, finish);
    else finish();
  }, [open, present]);

  useLayoutEffect(() => () => { generation.current++; animation.current?.cancel(); }, []);
  if (!present && !open) return null;
  return <div ref={root} className="motion-presence" inert={!open} aria-hidden={!open || undefined}>{children}</div>;
}
