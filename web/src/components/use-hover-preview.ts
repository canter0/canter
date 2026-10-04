"use client";

import { useCallback, useEffect, useRef, useState, type RefObject } from "react";
import { movingThroughHoverTriangle, pointInHoverRect, type PointerPoint } from "@/lib/hover-intent";

/** Keeps an interactive preview reachable across other rows and the menu gap. */
export function useHoverPreview(open: boolean, stacked: boolean, panel: RefObject<HTMLElement | null>, card: RefObject<HTMLElement | null>) {
  const [preview, setPreview] = useState<string | null>(null);
  const active = useRef<string | null>(null);
  const last = useRef<PointerPoint | null>(null);
  const origin = useRef<PointerPoint | null>(null);
  const pending = useRef<ReturnType<typeof setTimeout> | null>(null);
  const cancelPending = useCallback(() => {
    if (pending.current !== null) clearTimeout(pending.current);
    pending.current = null;
  }, []);
  const showPreview = useCallback((value: string | null, point?: PointerPoint) => {
    cancelPending();
    active.current = value;
    origin.current = point ?? last.current;
    setPreview(value);
  }, [cancelPending]);

  useEffect(() => {
    if (!open || stacked) return;
    function move(event: PointerEvent) {
      if (event.pointerType === "touch") return;
      const point = { x: event.clientX, y: event.clientY };
      const previous = last.current;
      last.current = point;
      const target = event.target instanceof Element ? event.target : null;
      const row = target?.closest<HTMLElement>("[data-model-option]");
      const candidate = row && panel.current?.contains(row) ? row.dataset.modelOption ?? null : null;
      const cardRect = card.current?.getBoundingClientRect();
      if (cardRect && pointInHoverRect(point, cardRect)) {
        cancelPending();
        origin.current = null;
        return;
      }
      if (candidate && candidate === active.current) {
        cancelPending();
        origin.current = point;
        return;
      }
      const side = cardRect && panel.current && cardRect.left < panel.current.getBoundingClientRect().left ? "left" : "right";
      if (active.current && cardRect && origin.current && previous && movingThroughHoverTriangle(point, previous, origin.current, cardRect, side)) {
        // Renew while moving toward the card, even for a slow diagonal path.
        // A pause on a different row should eventually preview that row.
        cancelPending();
        pending.current = setTimeout(() => showPreview(candidate, point), 350);
        return;
      }
      if (candidate) {
        showPreview(candidate, point);
        return;
      }
      if (card.current?.contains(document.activeElement)) {
        cancelPending();
        return;
      }
      // A small close grace also covers moving back across the physical gap.
      // Keyboard focus never goes through this pointer-only path.
      if (active.current && pending.current === null) pending.current = setTimeout(() => showPreview(null), 100);
    }
    function keyboard() {
      cancelPending();
      origin.current = null;
      last.current = null;
    }
    function pointerDown() { cancelPending(); }
    function leaveWindow(event: PointerEvent) {
      if (event.relatedTarget === null && !card.current?.contains(document.activeElement)) showPreview(null);
    }
    document.addEventListener("pointermove", move);
    document.addEventListener("keydown", keyboard, true);
    document.addEventListener("pointerdown", pointerDown, true);
    document.addEventListener("pointerout", leaveWindow);
    return () => {
      cancelPending();
      origin.current = null;
      last.current = null;
      document.removeEventListener("pointermove", move);
      document.removeEventListener("keydown", keyboard, true);
      document.removeEventListener("pointerdown", pointerDown, true);
      document.removeEventListener("pointerout", leaveWindow);
    };
  }, [open, stacked, panel, card, cancelPending, showPreview]);

  return [preview, showPreview] as const;
}
