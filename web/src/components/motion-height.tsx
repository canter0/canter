"use client";

import { useLayoutEffect, useRef, type ReactNode } from "react";
import { motion, playMotion } from "@/lib/motion";

/** Measure the natural inner content so our animated height never feeds itself. */
export function MotionHeight({ children, className }: { children: ReactNode; className?: string }) {
  const outer = useRef<HTMLDivElement>(null);
  const inner = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const element = outer.current;
    const content = inner.current;
    if (!element || !content) return;
    let height = content.getBoundingClientRect().height;
    let animation: Animation | null = null;
    const measure = () => {
      const next = content.getBoundingClientRect().height;
      if (Math.abs(next - height) < 1) return;
      const previous = animation ? element.getBoundingClientRect().height : height;
      height = next;
      animation?.cancel();
      element.style.overflow = "clip";
      animation = playMotion(element, [{ height: `${previous}px` }, { height: `${next}px` }], { duration: motion.layout });
      const current = animation;
      const finish = () => { if (animation === current) { animation = null; element.style.removeProperty("overflow"); } };
      if (animation) animation.finished.then(finish, finish);
      else finish();
    };
    const resize = new ResizeObserver(measure);
    resize.observe(content);
    return () => { resize.disconnect(); animation?.cancel(); element.style.removeProperty("overflow"); };
  }, []);
  return <div ref={outer} className={className}><div ref={inner} style={{ display: "flow-root" }}>{children}</div></div>;
}
