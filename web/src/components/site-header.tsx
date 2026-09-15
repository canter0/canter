import Link from "next/link";
import styles from "./site-header.module.css";

function LinkArrow() {
  return (
    <svg className={styles.arrow} width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M7 17 17 7M7 7h10v10" />
    </svg>
  );
}

export function SiteHeader() {
  return (
    <header className={styles.header}>
      <Link href="/" className={`wordmark ${styles.wordmark}`} aria-label="Canter home">canter</Link>
      <nav className={styles.navigation} aria-label="Main navigation">
        <Link href="/pricing" className={styles.secondaryLink}>Pricing</Link>
        <a href="https://github.com/canter0/canter#how-it-works" className={styles.secondaryLink}>Docs</a>
        <a href="https://github.com/canter0/canter" className={styles.secondaryLink}>GitHub <LinkArrow /></a>
      </nav>
      <div className={styles.actions}>
        <Link href="/sign-in" className={styles.signIn}>Sign in</Link>
        <Link href="/create-account" className={styles.getStarted}>Get started <LinkArrow /></Link>
      </div>
    </header>
  );
}
