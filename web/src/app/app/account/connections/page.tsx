import { SettingsShell } from "@/components/settings-shell";
import { ConnectedAccounts } from "@/components/connected-accounts";
import styles from "@/components/settings.module.css";
export default function ConnectionsPage() {
  return <SettingsShell active="Connections" title="Connections" description="Manage sign-in accounts and repository access."><section className={styles.section}><h2>Connected accounts</h2><div className={styles.card}><ConnectedAccounts /></div></section></SettingsShell>;
}
