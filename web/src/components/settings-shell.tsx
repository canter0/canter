"use client";

import Link from "next/link";
import { type ReactNode } from "react";
import { AppShell } from "./app-shell";
import { WorkspaceIcon } from "./workspace-icon";
import { SettingsNavigation } from "./settings-navigation";
import styles from "./settings.module.css";

export function SettingsShell({ active, title, description, children }: { active: string; title: string; description?: string; children: ReactNode }) {
  return <AppShell active={["Usage", "Plans", "Invoices"].includes(active) ? "Billing" : "Account"} settingsNavigation={<SettingsNavigation active={active} />}>
    <div className={styles.breadcrumb}><Link href="/app/settings">Settings</Link><WorkspaceIcon name="chevron" width="12" height="12" /><span>{title}</span></div>
    <div className={styles.page}><header className={styles.heading}><h1>{title}</h1>{description ? <p>{description}</p> : null}</header>{children}</div>
  </AppShell>;
}

export function SettingRow({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return <div className={styles.row}><div><h3>{title}</h3>{description ? <p>{description}</p> : null}</div><div className={styles.rowValue}>{children}</div></div>;
}
