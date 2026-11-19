"use client";

import { useEffect, useRef, useState } from "react";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import styles from "@/app/home.module.css";

const items: Array<{ label: string; icon: WorkspaceIconName }> = [
  { label: "Infrastructure", icon: "cloudServer" },
  { label: "Servers", icon: "serverStack" },
  { label: "Sandboxes", icon: "sandbox" },
  { label: "CI", icon: "pipeline" },
];

export function HeroProducts() {
  const [index, setIndex] = useState(0);
  const [previous, setPrevious] = useState<number | null>(null);
  const [widths, setWidths] = useState<number[]>([]);
  const [paused, setPaused] = useState(false);
  const words = useRef<Array<HTMLSpanElement | null>>([]);

  useEffect(() => {
    const measure = () => setWidths(words.current.map(word => word?.offsetWidth ?? 0));
    measure();
    document.fonts?.ready.then(measure);
    const observer = new ResizeObserver(measure);
    for (const word of words.current) if (word) observer.observe(word);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    const reduced = window.matchMedia("(prefers-reduced-motion: reduce)");
    let timer: number | undefined;
    const schedule = () => {
      window.clearInterval(timer);
      timer = undefined;
      if (reduced.matches) { setIndex(0); setPrevious(null); return; }
      if (paused || document.hidden) return;
      timer = window.setInterval(() => setIndex(current => { setPrevious(current); return (current + 1) % items.length; }), 2800);
    };
    schedule();
    reduced.addEventListener("change", schedule);
    document.addEventListener("visibilitychange", schedule);
    return () => {
      window.clearInterval(timer);
      reduced.removeEventListener("change", schedule);
      document.removeEventListener("visibilitychange", schedule);
    };
  }, [paused]);

  const state = (i: number) => i === index ? "active" : i === previous ? "out" : "idle";
  return (
    <span className={styles.rotator} aria-hidden="true" onPointerEnter={() => setPaused(true)} onPointerLeave={() => setPaused(false)}>
      <span className={styles.rotatorIcon}>
        {items.map((item, i) => <WorkspaceIcon key={item.label} name={item.icon} width="1em" height="1em" data-state={state(i)} />)}
      </span>
      <span className={styles.rotatorWords} style={widths[index] ? { width: widths[index] } : undefined}>
        {items.map((item, i) => <span key={item.label} ref={word => { words.current[i] = word; }} data-state={state(i)}>{item.label}</span>)}
      </span>
    </span>
  );
}
