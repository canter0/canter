"use client";

import { useEffect, useLayoutEffect, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { TextMorph } from "torph/react";
import styles from "./conversation-motion.module.css";
import chat from "./operator-workspace.module.css";
import { motion } from "@/lib/motion";

const EASE = motion.ease;
const SHIMMER_MS = 720;

export function MorphLabel({ text, shimmer = false, className = "" }: { text: string; shimmer?: boolean; className?: string }) {
  const slot = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    const element = slot.current;
    const preference = window.matchMedia("(prefers-reduced-motion: reduce)");
    if (!element || !shimmer) return;
    const animations = new Map<HTMLElement, { animation: Animation; geometry: string }>();
    let frame = 0;
    const synchronize = () => {
      if (preference.matches) {
        for (const entry of animations.values()) entry.animation.cancel();
        animations.clear();
        return;
      }
      const root = element.querySelector<HTMLElement>("[torph-root]");
      if (!root) return;
      const width = root.offsetWidth;
      if (!width) return;
      const items = [...root.querySelectorAll<HTMLElement>("[torph-item]")];
      // Measure together; each piece samples one continuous highlight band.
      const geometry = items.map(item => ({ item, offset: item.offsetLeft }));
      const phase = performance.now() % SHIMMER_MS;
      for (const { item, offset } of geometry) {
        const key = `${width}:${offset}`;
        if (animations.get(item)?.geometry === key) continue;
        animations.get(item)?.animation.cancel();
        item.style.backgroundSize = `${width * 2.5}px 100%`;
        const animation = item.animate([
          { backgroundPosition: `${-width * 1.5 - offset}px 0` },
          { backgroundPosition: `${-offset}px 0` },
        ], { duration: SHIMMER_MS, easing: "linear", iterations: Infinity });
        animation.currentTime = phase;
        animations.set(item, { animation, geometry: key });
      }
      for (const [item, entry] of animations) {
        if (!items.includes(item)) { entry.animation.cancel(); animations.delete(item); }
      }
    };
    const schedule = () => { cancelAnimationFrame(frame); frame = requestAnimationFrame(synchronize); };
    const observer = new MutationObserver(schedule);
    observer.observe(element, { childList: true, subtree: true });
    element.addEventListener("transitionend", schedule);
    preference.addEventListener("change", schedule);
    synchronize();
    schedule();
    return () => {
      observer.disconnect();
      element.removeEventListener("transitionend", schedule);
      preference.removeEventListener("change", schedule);
      cancelAnimationFrame(frame);
      for (const entry of animations.values()) entry.animation.cancel();
    };
  }, [shimmer]);
  return <span ref={slot} className={`${styles.label} ${className}`} data-shimmer={shimmer}>
    <TextMorph duration={motion.text} ease={EASE} scale={false} numbers={false}>{text}</TextMorph>
  </span>;
}

type TextNode = { type: string; value?: string; tagName?: string; properties?: Record<string, unknown>; children?: TextNode[]; position?: { start: { offset?: number } } };

// Wrap text leaves rather than flattening Markdown: links, emphasis, lists and
// selections keep their real semantics. Code and tables remain whole blocks.
function revealWords() {
  return (tree: TextNode) => {
    let fallback = 0;
    function visit(node: TextNode, excluded = false) {
      const skip = excluded || ["pre", "code", "table", "svg", "math"].includes(node.tagName ?? "");
      if (!node.children || skip) return;
      node.children = node.children.flatMap(child => {
        if (child.type !== "text" || !child.value) { visit(child, skip); return [child]; }
        const offset = child.position?.start.offset ?? fallback;
        fallback += child.value.length;
        return [...child.value.matchAll(/\S+|\s+/gu)].map(match => /^\s/u.test(match[0])
          ? { type: "text", value: match[0] }
          : { type: "element", tagName: "span", properties: { className: [styles.word], "data-reveal-word": String(offset + match.index) }, children: [{ type: "text", value: match[0] }] });
      });
    }
    visit(tree);
  };
}
const REVEAL_PLUGINS = [revealWords];

export function ResponseText({ text, streaming = false }: { text: unknown; streaming?: boolean }) {
  const content = typeof text === "string" ? text : "";
  const [visible, setVisible] = useState(streaming ? "" : content);
  const [revealing, setRevealing] = useState(streaming);
  const root = useRef<HTMLDivElement>(null);
  const shown = useRef(streaming ? "" : content);
  const animated = useRef(streaming);
  const previousText = useRef(streaming ? "" : content);
  const animations = useRef(new Set<Animation>());

  useEffect(() => {
    let frame = 0;
    let previous = 0;
    animated.current ||= streaming;
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)");
    const tick = (time: number) => {
      const elapsed = previous ? Math.min(time - previous, 64) : 32;
      if (previous && elapsed < 28) { frame = requestAnimationFrame(tick); return; }
      if (!animated.current || reduced.matches || document.hidden || !content.startsWith(shown.current)) {
        shown.current = content;
      } else {
        const remaining = content.length - shown.current.length;
        let end = Math.min(content.length, shown.current.length + Math.max(1, Math.ceil(remaining * elapsed / 160)));
        if (end < content.length && /[\uD800-\uDBFF]/.test(content[end - 1])) end++;
        shown.current = content.slice(0, end);
      }
      previous = time;
      setVisible(shown.current);
      if (shown.current !== content) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [content, streaming]);

  useEffect(() => {
    if (streaming) {
      const frame = requestAnimationFrame(() => setRevealing(true));
      return () => cancelAnimationFrame(frame);
    }
    if (!animated.current || visible !== content) return;
    // Shed the animation wrappers after the final wave; history stays plain.
    const timer = setTimeout(() => setRevealing(false), 700);
    return () => clearTimeout(timer);
  }, [streaming, visible, content]);

  useLayoutEffect(() => {
    const element = root.current;
    if (!element || !revealing) return;
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    const previous = previousText.current;
    previousText.current = visible;
    // Closing Markdown delimiters can rebuild earlier text nodes. Only reveal
    // newly appended text, so formatting never reanimates an already read word.
    if (!visible.startsWith(previous)) return;
    let index = 0;
    for (const word of element.querySelectorAll<HTMLElement>("[data-reveal-word]")) {
      if (Number(word.dataset.revealWord) < previous.length) continue;
      if (reduced || document.hidden) continue;
      const animation = word.animate([
        { opacity: 0, filter: "blur(5px)", transform: "translateY(4px)" },
        { opacity: 1, filter: "none", transform: "none" },
      ], { duration: 420, delay: Math.min(index++ * 28, 168), easing: EASE, fill: "backwards" });
      animations.current.add(animation);
      animation.onfinish = () => { animation.cancel(); animations.current.delete(animation); };
    }
  }, [visible, revealing]);

  useEffect(() => {
    const active = animations.current;
    const preference = window.matchMedia("(prefers-reduced-motion: reduce)");
    const cancel = () => { for (const animation of active) animation.cancel(); active.clear(); };
    const changed = () => { if (preference.matches) cancel(); };
    preference.addEventListener("change", changed);
    return () => { preference.removeEventListener("change", changed); cancel(); };
  }, []);

  if (!visible) return null;
  return <div ref={root} className={chat.markdown} data-streaming={streaming}>
    <Markdown remarkPlugins={[remarkGfm]} rehypePlugins={revealing ? REVEAL_PLUGINS : undefined} skipHtml disallowedElements={["img", "hr"]} components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> }}>{visible}</Markdown>
  </div>;
}
