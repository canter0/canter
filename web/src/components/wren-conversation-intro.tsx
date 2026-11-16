import type { Wren } from "@/lib/wrens";
import { WrenAvatar } from "./wren-avatar";
import styles from "./wren-conversation-intro.module.css";

export function WrenConversationIntro({ wren, onEdit }: { wren: Wren; onEdit?: () => void }) {
  return <section className={styles.intro} aria-label={`${wren.name}'s purpose`}>
    <WrenAvatar shape={wren.shape} palette={wren.palette} size={64} />
    <div className={styles.identity}><h2>{wren.name}</h2>{wren.role && <span>{wren.role}</span>}</div>
    {wren.purpose.trim() && <p className={styles.purpose}>{wren.purpose.trim()}</p>}
    {onEdit && <div className={styles.actions}><button type="button" className={styles.settings} onClick={onEdit}>{wren.purpose.trim() ? "View purpose & settings" : "Set a purpose"}</button></div>}
  </section>;
}
