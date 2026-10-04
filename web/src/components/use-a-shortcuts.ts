"use client";

import { useEffect, useEffectEvent, useSyncExternalStore } from "react";
import { bindAShortcuts } from "@/lib/a-shortcuts";

const preferenceKey = "canter:a-shortcuts";
let fallbackEnabled = true;
const subscribe = (notify: () => void) => {
  window.addEventListener("storage", notify);
  window.addEventListener("canter-shortcuts", notify);
  return () => { window.removeEventListener("storage", notify); window.removeEventListener("canter-shortcuts", notify); };
};
export function useShortcutPreference() {
  const enabled = useSyncExternalStore(subscribe, () => { try { return localStorage.getItem(preferenceKey) !== "off"; } catch { return fallbackEnabled; } }, () => true);
  const setEnabled = (value: boolean) => {
    fallbackEnabled = value;
    try { localStorage.setItem(preferenceKey, value ? "on" : "off"); } catch { /* Keep the preference in memory. */ }
    window.dispatchEvent(new Event("canter-shortcuts"));
  };
  return { enabled, setEnabled };
}

export function useAShortcuts(enabled: boolean, run: (key: string) => boolean) {
  const execute = useEffectEvent(run);
  useEffect(() => {
    if (!enabled) return;
    return bindAShortcuts(window, execute, event => {
      const path = event.composedPath();
      if (path.some(item => item instanceof HTMLElement && (item.isContentEditable || item.closest('input, textarea, select, [role="textbox"], [role="combobox"], [role="searchbox"], [role="slider"], [role="spinbutton"], [role="menu"]')))) return true;
      if (document.querySelector("dialog[open]")) return true;
      const modal = document.querySelector('[aria-modal="true"]');
      if (!modal) return false;
      // Drawers retain their own toggle and tab shortcuts while containing focus.
      if (!modal.hasAttribute("data-shortcut-navigation") && !modal.hasAttribute("data-workspace-surface")) return true;
      const key = event.key.toLowerCase();
      if (key === "a") return false;
      return modal.hasAttribute("data-shortcut-navigation") ? key !== "q" : modal.hasAttribute("data-workspace-surface") ? !["e", "[", "]", ..."123456789"].includes(key) : true;
    });
  }, [enabled]);
}
