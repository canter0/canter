import { MorphLabel } from "./conversation-motion";
import styles from "./workspace-loading.module.css";

export function WorkspaceLoading({ variant = "rows" }: { variant?: "conversation" | "cards" | "rows" }) {
  return <div className={styles.loading} data-variant={variant} role="status" aria-label="Loading workspace">
    <span className={styles.mark} aria-hidden="true" />
    <MorphLabel text="Loading workspace" shimmer />
  </div>;
}
