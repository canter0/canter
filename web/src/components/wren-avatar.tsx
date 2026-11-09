"use client";
import { useId, type CSSProperties } from "react";
import { wrenPalettes, wrenState } from "@/lib/wrens";
import styles from "./wren-avatar.module.css";

function outline(shape: string) {
  const points = Array.from({ length: 96 }, (_, i) => {
    const a = i / 96 * Math.PI * 2, c = Math.cos(a), s = Math.sin(a);
    let x = c, y = s;
    if (shape === "square" || shape === "capsule") { x = Math.sign(c) * Math.abs(c) ** .48 * .93; y = Math.sign(s) * Math.abs(s) ** .48 * (shape === "capsule" ? .66 : .93); }
    if (shape === "pebble") { const r = 1 + .045 * Math.sin(a * 3 + .5); x = c * r; y = s * r * .94; }
    if (shape === "flower" || shape === "clover" || shape === "spark" || shape === "triangle") { const lobes = shape === "flower" ? 6 : shape === "triangle" ? 3 : 4; const r = (shape === "spark" ? .77 : .9) + (shape === "spark" ? .22 : .11) * Math.cos(lobes * a + (shape === "triangle" ? Math.PI / 2 : 0)); x = c * r; y = s * r; }
    return `${i ? "L" : "M"}${(32 + x * 27).toFixed(2)} ${(32 + y * 27).toFixed(2)}`;
  });
  return points.join("") + "Z";
}
export function WrenAvatar({ shape = "pebble", palette = 0, status = "idle", size = 40, animated = true }: { shape?: string; palette?: number; status?: string; size?: number; animated?: boolean }) {
  const id = useId().replaceAll(":", ""), colors = wrenPalettes[palette] ?? wrenPalettes[0], state = wrenState(status);
  return <svg width={size} height={size} viewBox="0 0 64 64" aria-hidden="true" className={styles.avatar} data-state={state} data-animated={animated} style={{ "--phase": `${-palette * .8}s` } as CSSProperties}>
    <defs><linearGradient id={`wren-fill-${id}`} x1="8" y1="5" x2="55" y2="58" gradientUnits="userSpaceOnUse"><stop stopColor={colors[0]} /><stop offset="1" stopColor={colors[1]} /></linearGradient></defs>
    <g className={styles.body}><path d={outline(shape)} fill={`url(#wren-fill-${id})`} /><g className={styles.gaze}><g className={styles.eyes} fill="#fffaf0"><rect x="24" y="25" width="4" height={state === "blocked" ? 6 : 11} rx="2" /><rect x="36" y="24" width="4" height="11" rx="2" /></g></g></g>
  </svg>;
}
