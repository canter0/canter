"use client";

import Link from "next/link";
import { type ReactNode } from "react";
import { AppShell } from "./app-shell";
import { WorkspaceIcon } from "./workspace-icon";
import { SettingsNavigation } from "./settings-navigation";
import { MorphLabel } from "./conversation-motion";
import styles from "./settings.module.css";

export function SettingsShell({ active, title, description, children }: { active: string; title: string; description?: string; children: ReactNode }) {
  return <AppShell active={["Usage", "Plans", "Invoices"].includes(active) ? "Billing" : "Account"} pageTitle={`${title} settings`} settingsNavigation={<SettingsNavigation active={active} />}>
    <nav className={styles.breadcrumb} aria-label="Breadcrumb">
      <ol><li><Link href="/app/settings">Settings</Link></li><li aria-hidden="true"><WorkspaceIcon name="chevron" width="12" height="12" /></li><li><span aria-current="page">{title}</span></li></ol>
    </nav>
    <div className={styles.page} data-settings-page={active}><header className={styles.heading} data-motion="reveal"><h1><MorphLabel text={title} /></h1>{description ? <p>{description}</p> : null}</header>{children}</div>
  </AppShell>;
}

export function SettingRow({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return <div className={styles.row}><div className={styles.rowLabel}><h3>{title}</h3>{description ? <p>{description}</p> : null}</div><div className={styles.rowValue}>{children}</div></div>;
}
