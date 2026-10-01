"use client";

import { useEffect, type RefObject } from "react";

/** Preserve the trigger even when React removes the dialog before effect cleanup. */
export function useDialog(ref: RefObject<HTMLDialogElement | null>, initialFocus?: string) {
  useEffect(() => {
    const element = ref.current;
    const trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    element?.showModal();
    if (initialFocus) element?.querySelector<HTMLElement>(initialFocus)?.focus();
    return () => {
      element?.close();
      requestAnimationFrame(() => { if (trigger?.isConnected) trigger.focus({ preventScroll: true }); });
    };
  }, [ref, initialFocus]);
}
