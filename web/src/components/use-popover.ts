"use client";

import { useEffect, type RefObject } from "react";

/** Native tab order is retained; Escape returns focus, outside click does not steal it. */
export function usePopover(open: boolean, root: RefObject<HTMLElement | null>, close: () => void) {
  useEffect(() => {
    if (!open) return;
    const element = root.current;
    const trigger = element?.querySelector<HTMLButtonElement>("button[aria-expanded]");
    element?.querySelector<HTMLElement>('[role^="menuitem"]')?.focus();
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !element?.contains(event.target)) close();
    };
    const keyboard = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); close(); trigger?.focus(); }
      if (event.key === "Tab") { close(); trigger?.focus(); }
    };
    document.addEventListener("pointerdown", outside);
    element?.addEventListener("keydown", keyboard);
    return () => { document.removeEventListener("pointerdown", outside); element?.removeEventListener("keydown", keyboard); };
  }, [open, root, close]);
}
