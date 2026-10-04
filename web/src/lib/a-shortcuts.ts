/** A is held like a modifier; releasing it never leaves a pending shortcut. */
export function bindAShortcuts(target: Pick<Window, "addEventListener" | "removeEventListener">, run: (key: string) => boolean, blocked: (event: KeyboardEvent) => boolean) {
  let held = false;
  const consumed = new Set<string>();
  const reset = () => { held = false; consumed.clear(); };
  const down = (event: KeyboardEvent) => {
    const key = event.key.toLowerCase();
    if (consumed.has(event.code || key)) { event.preventDefault(); return; }
    if (event.defaultPrevented || event.isComposing || event.keyCode === 229 || event.metaKey || event.ctrlKey || event.altKey || event.shiftKey || blocked(event)) { held = false; return; }
    if (key === "a") {
      if (!event.repeat) held = true;
      if (held) event.preventDefault();
      return;
    }
    if (!held || event.repeat) return;
    if (run(key)) { consumed.add(event.code || key); event.preventDefault(); }
  };
  const up = (event: KeyboardEvent) => {
    if (event.key.toLowerCase() === "a") held = false;
    if (consumed.delete(event.code || event.key.toLowerCase())) event.preventDefault();
  };
  target.addEventListener("keydown", down);
  target.addEventListener("keyup", up);
  target.addEventListener("blur", reset);
  target.addEventListener("pagehide", reset);
  return () => {
    target.removeEventListener("keydown", down);
    target.removeEventListener("keyup", up);
    target.removeEventListener("blur", reset);
    target.removeEventListener("pagehide", reset);
  };
}

export const shortcutPages = [
  { id: "apps", title: "Apps", key: "s", href: "/app/system", icon: "apps" },
  { id: "activity", title: "Activity", key: "d", href: "/app/changes", icon: "activity" },
  { id: "agents", title: "Agents", key: "g", href: "/app/agents", icon: "agent" },
  { id: "billing", title: "Billing", key: "b", href: "/app/billing", icon: "file" },
  { id: "settings", title: "Workspace settings", key: "w", href: "/app/settings", icon: "settings" },
  { id: "profile", title: "Profile", key: "p", href: "/app/account", icon: "agent" },
  { id: "security", title: "Account security", href: "/app/account/security", icon: "lock" },
  { id: "connections", title: "Connected accounts", href: "/app/account/connections", icon: "apps" },
  { id: "plans", title: "Plans", href: "/app/billing?view=plans", icon: "panel" },
  { id: "invoices", title: "Invoices", href: "/app/billing?view=invoices", icon: "file" },
  { id: "secrets", title: "Workspace secrets", href: "/app/settings/secrets", icon: "lock" },
  { id: "agent-settings", title: "Agent settings", href: "/app/settings/agent", icon: "agent" },
] as const;

export const shortcutLabel = (key: string) => `A + ${key === " " ? "Space" : key.toUpperCase()}`;
