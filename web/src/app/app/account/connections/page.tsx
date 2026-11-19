import Link from "next/link";
import { SettingsShell } from "@/components/settings-shell";
import { SignInAccounts, RepositoryAccess, AppAccountConnections } from "@/components/connected-accounts";
import { appsPageHref } from "@/lib/wren-apps";
import styles from "@/components/settings.module.css";

export default function ConnectionsPage() {
  return <SettingsShell active="Connections" title="Connections" description="Manage sign-in accounts, repository access, and personal app accounts.">
    <section className={styles.section}>
      <h2>Sign-in</h2>
      <p className="text-sm text-[var(--muted)]">Accounts you can use to sign in to Canter. Only you can use them.</p>
      <div className={styles.card}><SignInAccounts /></div>
    </section>
    <section className={styles.section}>
      <h2>GitHub repository access</h2>
      <p className="text-sm text-[var(--muted)]">Read repositories using your GitHub account. Your chats and Wrens use this connection for your requests; teammates connect their own.</p>
      <div className={styles.card}><RepositoryAccess /></div>
    </section>
    <section className={styles.section}>
      <h2>Linear accounts</h2>
      <p className="text-sm text-[var(--muted)]">Personal read-only Linear accounts for apps you install. Your account is used only for your requests, never for unattended work.</p>
      <div className={styles.card}><AppAccountConnections /></div>
    </section>
    <p className="text-sm text-[var(--muted)]"><Link className="rule-link" href={appsPageHref()}>Manage apps in Customize</Link> — pick which chat or room can use each connection.</p>
  </SettingsShell>;
}
