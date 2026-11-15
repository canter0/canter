"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { WorkspaceIcon } from "./workspace-icon";
import { WrenMark } from "./wren-mark";
import { useWorkspace } from "./workspace-context";
import { quicknavItems } from "../lib/quicknav-items";
import styles from "./quicknav.module.css";

export function Quicknav({ className = "", inert, onNavigate }: { className?: string; inert?: boolean; onNavigate?: () => void }) {
  const pathname = usePathname();
  const { data } = useWorkspace();
  const items = quicknavItems(data?.products);
  return <nav className={`${styles.rail} ${className}`} aria-label="Quicknav" inert={inert}>
    {items.map(item => {
      const current = item.href === "/app" ? !pathname.startsWith("/app/wrens") && !pathname.startsWith("/app/settings") && !pathname.startsWith("/app/account") && !pathname.startsWith("/app/billing") && !pathname.startsWith("/app/sandboxes") && !pathname.startsWith("/app/products") : pathname.startsWith(item.href);
      return <Link key={item.href} href={item.href} prefetch={item.href === "/app"} aria-label={item.label} title={item.label} aria-current={current ? "location" : undefined} onNavigate={onNavigate}>{item.wren ? <WrenMark /> : <WorkspaceIcon name={item.icon ?? "apps"} width="20" height="20" />}</Link>;
    })}
  </nav>;
}
