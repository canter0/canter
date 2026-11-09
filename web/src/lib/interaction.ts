import type { KeyboardEvent } from "react";

/** Menus contain one tab stop; arrows move between enabled items. */
export function moveMenuFocus(event: KeyboardEvent<HTMLElement>) {
  if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
  const items = Array.from(event.currentTarget.querySelectorAll<HTMLElement>('[role^="menuitem"]:not(:disabled)'));
  if (!items.length) return;
  event.preventDefault();
  const current = items.indexOf(document.activeElement as HTMLElement);
  const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 : (current + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
  items[next]?.focus();
}

export function focusableElements(container: HTMLElement) {
  return Array.from(container.querySelectorAll<HTMLElement>('a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), summary, [tabindex="0"]'))
    .filter(element => {
      if (element.closest('[inert], [hidden]') || !element.getClientRects().length) return false;
      // Closed details can still report layout boxes for their descendants.
      // Only the disclosure summary participates in keyboard navigation.
      for (let parent = element.parentElement; parent && container.contains(parent); parent = parent.parentElement) {
        if (parent instanceof HTMLDetailsElement && !parent.open) {
          const summary = Array.from(parent.children).find(child => child.tagName === "SUMMARY");
          if (!summary?.contains(element)) return false;
        }
      }
      return true;
    });
}
