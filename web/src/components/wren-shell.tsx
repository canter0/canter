"use client";
import Link from "next/link";
import {usePathname} from "next/navigation";
import type { ReactNode } from "react";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-shell.module.css";

export function WrenShell({
  title,
  icon,
  actions,
  onDetails,
  children,
}: {
  title: string;
  icon?: ReactNode;
  actions?: ReactNode;
  onDetails?: () => void;
  children: ReactNode;
}) {
  const pathname=usePathname();
  return (
    <div className={`dashboard-theme ${styles.shell}`}>
      <a className={styles.skip} href="#workspace-main">
        Skip to conversation
      </a>
      <header className={styles.header}>
        {pathname!=="/app/wrens" ? <Link
          href="/app/wrens"
          className={styles.back}
          aria-label="Back to conversations"
          title="Conversations"
        >
          <WorkspaceIcon
            name="right"
            style={{ transform: "rotate(180deg)" }}
            width="18"
            height="18"
          />
        </Link> : null}
        <div className={styles.identity}>
          {onDetails ? (
            <button
              type="button"
              data-page-title-action
              aria-label={`${title} details`}
              onClick={onDetails}
            >
              {icon}
              <h1>{title}</h1>
            </button>
          ) : (
            <>
              {icon}
              <h1>{title}</h1>
            </>
          )}
        </div>
        <div className={styles.actions}>
          {actions}
          <Link
            href="/app/settings"
            aria-label="Workspace settings"
            title="Workspace settings"
            className={styles.settings}
          >
            <WorkspaceIcon name="gear" width="17" height="17" />
          </Link>
        </div>
      </header>
      <main className={styles.main} id="workspace-main" tabIndex={-1}>
        {children}
      </main>
    </div>
  );
}
