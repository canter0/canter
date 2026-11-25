"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import { SettingsShell } from "./settings-shell";
import { WorkspaceIcon } from "./workspace-icon";
import keys from "./account-api-keys.module.css";
import styles from "./settings.module.css";

const listLimit = 100;

export function AccountAPIOverview() {
  const [count, setCount] = useState<number | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    void canterFetch<{ keys: unknown[] }>("/me/api-keys", { signal: controller.signal })
      .then(({ keys }) => {
        // A full response may have been truncated, so it cannot supply a count.
        if (!controller.signal.aborted && keys.length < listLimit) setCount(keys.length);
      })
      .catch(() => {}); // The issued keys page handles list failures and retry.
    return () => controller.abort();
  }, []);

  return <SettingsShell active="API" title="API" description="Use the Canter API to build tools and workflows.">
    <section className={styles.section}>
      <h2>API keys</h2>
      <div className={styles.card}>
        <Link href="/app/account/api-keys" className={`${styles.row} ${keys.overviewRow}`}>
          <div className={styles.rowLabel}>
            <h3>Issued keys</h3>
            <p>View and manage your personal API keys</p>
          </div>
          <span className={keys.overviewValue}>
            {count}
            <WorkspaceIcon name="chevron" width="16" height="16" />
          </span>
        </Link>
      </div>
    </section>
  </SettingsShell>;
}
