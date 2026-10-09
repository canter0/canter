"use client";

import type {} from "react/canary";
import { useLayoutEffect, ViewTransition, type ReactNode } from "react";
import { usePathname } from "next/navigation";
import { motion, playMotion } from "@/lib/motion";

export function ContentTransition({ name, children }: { name: string; children: ReactNode }) {
  return <ViewTransition name={name} default="none" update="canter-page"><div>{children}</div></ViewTransition>;
}

/** Public route snapshots; workspace routes animate their content inside the shell. */
export function SiteTransition({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  if (pathname.startsWith("/app")) return children;
  return <ViewTransition key={pathname} name="canter-public-page" default="none" share="canter-page" enter="canter-page" exit="canter-page">
    <div data-motion-page>{children}</div>
  </ViewTransition>;
}

/** One observer for deliberate reveals and one press response for all controls. */
export function SiteMotion() {
  useLayoutEffect(() => {
    const transitions = CSS.supports("selector(:active-view-transition)");
    const animations = new Set<Animation>();
    const observed = new WeakSet<Element>();
    const pressed = new Map<HTMLElement, Animation | null>();
    const track = (animation: Animation | null) => {
      if (animation) {
        animations.add(animation);
        const remove = () => animations.delete(animation);
        animation.addEventListener("finish", remove, { once: true });
        animation.addEventListener("cancel", remove, { once: true });
      }
      return animation;
    };
    const reveal = (element: HTMLElement) => {
      // Route snapshots already supply the reveal. Avoid freezing a partly
      // animated child in a snapshot or replaying motion on streamed chat text.
      if (transitions && document.documentElement.matches(":active-view-transition")) return;
      const fade = element.dataset.motion === "fade";
      track(playMotion(element, [
        { opacity: 0, ...(fade ? {} : { filter: "blur(5px)", transform: "translateY(7px)" }) },
        { opacity: 1, ...(fade ? {} : { filter: "none", transform: "none" }) },
      ], { duration: motion.reveal, delay: Math.min(Number(element.dataset.motionDelay) || 0, 240) }));
    };
    const viewport = new IntersectionObserver(entries => {
      for (const entry of entries) if (entry.isIntersecting) {
        viewport.unobserve(entry.target);
        reveal(entry.target as HTMLElement);
      }
    }, { threshold: 0.08 });
    const discover = (root: Element) => {
      const elements = root.childElementCount ? [...root.querySelectorAll<HTMLElement>('[data-motion="reveal"], [data-motion="fade"]')] : [];
      if (root.matches('[data-motion="reveal"], [data-motion="fade"]')) elements.unshift(root as HTMLElement);
      for (const element of elements) {
        if (observed.has(element)) continue;
        observed.add(element);
        const rect = element.getBoundingClientRect();
        if (rect.top < window.innerHeight && rect.bottom > 0) reveal(element);
        else viewport.observe(element);
      }
    };
    discover(document.body);
    const observer = new MutationObserver(records => {
      for (const record of records) for (const node of record.addedNodes) {
        if (node instanceof Element) discover(node);
      }
    });
    observer.observe(document.body, { childList: true, subtree: true });

    const control = (target: EventTarget | null) => {
      if (!(target instanceof Element)) return null;
      const element = target.closest<HTMLElement>('button, summary, [role="button"], a[class]');
      if (!element || element.matches(':disabled, [aria-disabled="true"]') || element.closest('[inert], [data-motion-nav], [data-motion-press="off"]')) return null;
      return element;
    };
    const press = (element: HTMLElement | null) => {
      if (!element || pressed.has(element)) return;
      const scale = getComputedStyle(element).scale;
      pressed.set(element, track(playMotion(element, [{ scale: scale === "none" ? "1" : scale }, { scale: "0.975" }], { duration: motion.press, fill: "forwards" })));
    };
    const release = () => {
      for (const [element, animation] of pressed) {
        const scale = getComputedStyle(element).scale;
        animation?.cancel();
        track(playMotion(element, [{ scale: scale === "none" ? "1" : scale }, { scale: "1" }], { duration: motion.layout }));
      }
      pressed.clear();
    };
    const pointer = (event: PointerEvent) => { if (event.button === 0) press(control(event.target)); };
    const keyboard = (event: KeyboardEvent) => {
      if (event.repeat || event.isComposing || (event.key !== "Enter" && event.key !== " ")) return;
      const element = control(event.target);
      if (event.key === " " && element?.tagName === "A") return;
      press(element);
    };
    const keyup = (event: KeyboardEvent) => { if (event.key === "Enter" || event.key === " ") release(); };
    const preference = window.matchMedia("(prefers-reduced-motion: reduce)");
    const changed = () => { if (preference.matches) release(); };
    document.addEventListener("pointerdown", pointer);
    document.addEventListener("pointerup", release);
    document.addEventListener("pointercancel", release);
    document.addEventListener("keydown", keyboard);
    document.addEventListener("keyup", keyup);
    window.addEventListener("blur", release);
    preference.addEventListener("change", changed);
    return () => {
      observer.disconnect(); viewport.disconnect();
      document.removeEventListener("pointerdown", pointer);
      document.removeEventListener("pointerup", release);
      document.removeEventListener("pointercancel", release);
      document.removeEventListener("keydown", keyboard);
      document.removeEventListener("keyup", keyup);
      window.removeEventListener("blur", release);
      preference.removeEventListener("change", changed);
      for (const animation of animations) animation.cancel();
      for (const animation of pressed.values()) animation?.cancel();
    };
  }, []);
  return null;
}
