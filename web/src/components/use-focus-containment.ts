"use client";

import { useEffect, type RefObject } from "react";
import { focusableElements } from "@/lib/interaction";

export function useFocusContainment(open: boolean, root: RefObject<HTMLElement | null>, close: () => void, returnFocus?: RefObject<HTMLElement | null>) {
  useEffect(() => {
    const element = root.current;
    if (!open || !element) return;
    const previous = document.activeElement instanceof HTMLElement && document.activeElement !== document.body && !element.contains(document.activeElement) ? document.activeElement : returnFocus?.current;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    // Make the entire background unavailable, including navigation outside the
    // panel's immediate parent. Only restore attributes owned by this effect.
    const background: HTMLElement[] = [];
    let branch: HTMLElement | null = element;
    while (branch?.parentElement) {
      for (const sibling of Array.from(branch.parentElement.children)) {
        if (sibling instanceof HTMLElement && sibling !== branch && !sibling.inert) { sibling.inert = true; background.push(sibling); }
      }
      if (branch.parentElement === document.body) break;
      branch = branch.parentElement;
    }
    const focusInside = () => (focusableElements(element)[0] ?? element).focus();
    focusInside();
    // Reconcile focus after opening styles settle without moving it if the
    // user is already inside.
    const initialFocus = requestAnimationFrame(() => {
      if (!element.contains(document.activeElement)) focusInside();
    });
    const keyboard = (event: KeyboardEvent) => {
      // Native modal dialogs own focus and Escape while they are on top of a drawer.
      if (event.target instanceof Element && event.target.closest("dialog[open]")) return;
      if (event.key === "Escape" && !event.defaultPrevented) { event.preventDefault(); close(); }
      if (event.key !== "Tab") return;
      const items = focusableElements(element);
      const first = items[0];
      const last = items.at(-1);
      if (!first) { event.preventDefault(); element.focus(); return; }
      if (event.shiftKey && (document.activeElement === first || !element.contains(document.activeElement))) { event.preventDefault(); last?.focus(); }
      else if (!event.shiftKey && (document.activeElement === last || !element.contains(document.activeElement))) { event.preventDefault(); first.focus(); }
    };
    document.addEventListener("keydown", keyboard);
    return () => {
      cancelAnimationFrame(initialFocus);
      document.removeEventListener("keydown", keyboard);
      document.body.style.overflow = overflow;
      for (const sibling of background) sibling.inert = false;
      requestAnimationFrame(() => { if (previous?.isConnected && !previous.closest("[inert]")) previous.focus({ preventScroll: true }); });
    };
  }, [open, root, close, returnFocus]);
}
