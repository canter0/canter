"use client";

import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties, type KeyboardEvent } from "react";
import { operatorModelChoice, operatorModels, operatorModelOptions, reasoningLabels, type OperatorModelChoice, type OperatorModelOptions } from "@/lib/operator-models";
import { useHoverPreview } from "./use-hover-preview";
import { WorkspaceIcon } from "./workspace-icon";
import shared from "./workspace.module.css";
import styles from "./operator-model-picker.module.css";

// Original SVG marks from @lobehub/icons-static-svg 1.95.1 (MIT).
// https://github.com/lobehub/lobe-icons — license bundled in /model-logos/LICENSE.
const logos: Record<string, string> = { openai: "openai.svg", zai: "zai.svg", deepseek: "deepseek-color.svg", qwen: "qwen-color.svg", gemini: "gemini-color.svg" };
function ModelLogo({ brand }: { brand: string }) {
  if (!logos[brand]) return <WorkspaceIcon name="agent" width="18" height="18" />;
  return <span aria-hidden="true" className={styles.logo} data-mono={brand === "openai" || brand === "zai"} style={{ "--model-logo": `url(/model-logos/${logos[brand]})` } as CSSProperties} />;
}

type Props = { model?: string; onChange: (model: string) => void; options: Record<string, OperatorModelOptions>; onOptionsChange: (model: string, options: OperatorModelOptions) => void; open: boolean; onOpenChange: (open: boolean) => void; disabled?: boolean };
export function OperatorModelPicker({ model, onChange, options, onOptionsChange, open, onOpenChange, disabled }: Props) {
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const search = useRef<HTMLInputElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const card = useRef<HTMLElement>(null);
  const id = useId();
  const [query, setQuery] = useState("");
  const [position, setPosition] = useState({ left: 0, bottom: 0, stacked: false, side: "right", below: false, maxHeight: 280 });
  const [preview, setPreview] = useHoverPreview(open, position.stacked, panel, card);
  const current = operatorModelChoice(model);
  const choices = current && !operatorModels.some(item => item.id === current.id) ? [current, ...operatorModels] : operatorModels;
  const filtered = choices.filter(item => `${item.name} ${item.provider}`.toLowerCase().includes(query.trim().toLowerCase()));
  const inspected = filtered.find(item => item.id === preview);
  const settings = operatorModelOptions(inspected?.id, inspected ? options[inspected.id] : undefined);

  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const rect = trigger.current?.getBoundingClientRect();
      if (!rect) return;
      const viewport = window.visualViewport;
      const width = viewport?.width ?? window.innerWidth;
      const height = viewport?.height ?? window.innerHeight;
      const viewTop = viewport?.offsetTop ?? 0;
      const viewLeft = viewport?.offsetLeft ?? 0;
      const left = Math.max(viewLeft + 10, Math.min(rect.left, viewLeft + width - 234));
      const fitsRight = left + 454 <= viewLeft + width - 10;
      const fitsLeft = left - 230 >= viewLeft + 10;
      const stacked = !fitsRight && !fitsLeft;
      const anchorBottom = Math.min(rect.top - 8, viewTop + height - 10);
      const bottom = window.innerHeight - anchorBottom;
      setPosition({ left, bottom, stacked, side: fitsRight ? "right" : "left", below: viewTop + height - anchorBottom > 205, maxHeight: Math.max(80, anchorBottom - viewTop - 50) });
    };
    place();
    window.addEventListener("resize", place);
    window.visualViewport?.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => { window.removeEventListener("resize", place); window.visualViewport?.removeEventListener("resize", place); window.removeEventListener("scroll", place, true); };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    search.current?.focus();
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !root.current?.contains(event.target)) onOpenChange(false); };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open, onOpenChange]);

  function close() { onOpenChange(false); trigger.current?.focus(); }
  function choose(choice: OperatorModelChoice) { onChange(choice.id); close(); }
  function keyboard(event: KeyboardEvent) {
    if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); close(); return; }
    if (event.target instanceof HTMLSelectElement) return;
    const options = Array.from(panel.current?.querySelectorAll<HTMLButtonElement>('[role="option"]') ?? []);
    const index = options.indexOf(document.activeElement as HTMLButtonElement);
    if (["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault();
      const next = index < 0 ? (event.key === "ArrowDown" ? 0 : options.length - 1) : (index + (event.key === "ArrowDown" ? 1 : options.length - 1)) % options.length;
      options[next]?.focus();
    } else if (index >= 0 && ["Home", "End"].includes(event.key)) {
      event.preventDefault(); options[event.key === "Home" ? 0 : options.length - 1]?.focus();
    } else if (event.key === "Enter" && event.target === search.current && filtered[0]) {
      event.preventDefault(); choose(filtered[0]);
    }
  }

  return <div ref={root} className={styles.root} onBlur={event => { if (event.relatedTarget && !event.currentTarget.contains(event.relatedTarget)) onOpenChange(false); }}>
    <button ref={trigger} type="button" className={shared.modelTrigger} aria-label={`Model: ${current?.name ?? "Connecting"}`} aria-haspopup="dialog" aria-controls={open ? id : undefined} aria-expanded={open} disabled={disabled || !model} onClick={() => { setQuery(""); setPreview(null); onOpenChange(!open); }}><span className={styles.triggerLabel}>{current?.name ?? "Connecting…"}</span><WorkspaceIcon name="down" width="12" height="12" /></button>
    {open ? <div ref={panel} id={id} role="dialog" aria-label="Choose a model" className={styles.picker} data-stacked={position.stacked} data-side={position.side} data-below={position.below} style={{ left: position.left, bottom: position.bottom, "--picker-max-height": `${position.maxHeight}px` } as CSSProperties} onKeyDown={keyboard}>
      <div className={styles.menu}>
        <div className={styles.search}><WorkspaceIcon name="search" width="16" height="16" /><input ref={search} aria-label="Search models" placeholder="Search models" value={query} onChange={event => { setQuery(event.target.value); setPreview(null); }} aria-controls={`${id}-options`} onFocus={() => setPreview(null)} /></div>
        <div className={styles.list} role="listbox" id={`${id}-options`} aria-label="Models">
          {filtered.map(choice => <button key={choice.id} type="button" role="option" aria-selected={choice.id === model} aria-describedby={inspected?.id === choice.id ? `${id}-details` : undefined} tabIndex={choice.id === (inspected?.id ?? filtered.find(item => item.id === model)?.id ?? filtered[0]?.id) ? 0 : -1} className={styles.option} data-model-option={choice.id} data-preview={choice.id === inspected?.id} onFocus={() => setPreview(choice.id)} onClick={event => { if (position.stacked && event.detail !== 0) setPreview(choice.id); else choose(choice); }}><ModelLogo brand={choice.logo} /><span>{choice.name}</span>{choice.id === model ? <WorkspaceIcon name="check" width="14" height="14" /> : null}</button>)}
          {!filtered.length ? <p className={styles.empty} role="status">No models found.</p> : null}
        </div>
      </div>
      {inspected ? <aside ref={card} data-model-settings className={styles.details} id={`${id}-details`} aria-label={`${inspected.name} details`}>
        <div className={styles.heading}><ModelLogo brand={inspected.logo} /><strong>{inspected.name}</strong></div>
        {inspected.contextTokens ? <p className={styles.context}>{new Intl.NumberFormat("en", { maximumFractionDigits: 2 }).format(inspected.contextTokens / 1_000_000)}M context</p> : null}
        {inspected.reasoningEfforts ? <div className={styles.controls}>
          <label className={styles.control}><span>Reasoning</span><span className={styles.reasoningSelect}><select aria-label={`Reasoning for ${inspected.name}`} value={settings.reasoningEffort} disabled={disabled} onChange={event => onOptionsChange(inspected.id, { ...settings, reasoningEffort: event.target.value })}>{inspected.reasoningEfforts.map(effort => <option key={effort} value={effort}>{reasoningLabels[effort]}</option>)}</select><WorkspaceIcon name="down" width="12" height="12" /></span></label>
        </div> : null}
        {position.stacked ? <button type="button" className={styles.choose} onClick={() => choose(inspected)}>Use {inspected.name}</button> : null}
      </aside> : null}
    </div> : null}
  </div>;
}
