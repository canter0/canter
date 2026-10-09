"use client";

import { useCallback, useEffect, useRef, type RefObject } from "react";
import { motion, playMotion } from "@/lib/motion";

/** Preserve the trigger even when React removes the dialog before effect cleanup. */
export function useDialog(ref: RefObject<HTMLDialogElement | null>, initialFocus?: string) {
  const closing = useRef<Animation | null>(null);
  useEffect(() => {
    const element = ref.current;
    const trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    element?.showModal();
    if (initialFocus) element?.querySelector<HTMLElement>(initialFocus)?.focus();
    return () => {
      closing.current?.cancel();
      element?.close();
      requestAnimationFrame(() => { if (trigger?.isConnected) trigger.focus({ preventScroll: true }); });
    };
  }, [ref, initialFocus]);
  return useCallback((onClose: () => void) => {
    const element = ref.current;
    if (!element || !element.open) { onClose(); return; }
    if (closing.current) return;
    element.dataset.motionClosing = "true";
    element.inert = true;
    element.setAttribute("aria-hidden", "true");
    const current = getComputedStyle(element);
    const animation = playMotion(element, [
      { opacity: current.opacity, filter: current.filter, transform: current.transform },
      { opacity: 0, filter: "blur(3px)", transform: "translateY(4px) scale(.985)" },
    ], { duration: motion.exit, fill: "forwards" });
    closing.current = animation;
    if (!animation) { onClose(); return; }
    animation.finished.then(onClose, () => {
      // A preference change completes dismissal; unmount cleanup does not.
      if (element.isConnected && window.matchMedia("(prefers-reduced-motion: reduce)").matches) onClose();
    });
  }, [ref]);
}
