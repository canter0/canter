import { WorkspaceIcon } from "./workspace-icon";
import styles from "./landing-capabilities.module.css";

export function SystemTree({ name = "stateful-board", className = "" }: { name?: string; className?: string }) {
  return (
    <div className={`${styles.systemTree} ${className}`} role="group" aria-label="Example System">
      <div className={styles.systemRoot}>
        <WorkspaceIcon name="stack" />
        <span>{name}</span>
      </div>
      <ul className={styles.systemResources}>
        <li><WorkspaceIcon name="serverStack" />Web service</li>
        <li>
          <svg aria-hidden="true" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round">
            <ellipse cx="12" cy="5.5" rx="7" ry="3" />
            <path d="M5 5.5v13c0 1.7 3.1 3 7 3s7-1.3 7-3v-13M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3" />
          </svg>
          Postgres
        </li>
        <li><WorkspaceIcon name="globe" />HTTPS endpoint</li>
      </ul>
    </div>
  );
}

export function BuildPipeline() {
  return (
    <ol className={styles.pipeline} aria-label="Example workflow">
      <li><WorkspaceIcon name="gear" />Build</li>
      <li><span className={styles.check}><WorkspaceIcon name="check" width={14} height={14} /></span>Test</li>
    </ol>
  );
}

function ReleaseTable() {
  return (
    <div className={styles.releaseTable}>
      <table>
        <caption className="sr-only">Example release history</caption>
        <thead>
          <tr><th scope="col">Release</th><th scope="col">Status</th></tr>
        </thead>
        <tbody>
          <tr>
            <th scope="row"><code>v12</code></th>
            <td><span className={styles.live}>Live</span></td>
          </tr>
          <tr>
            <th scope="row"><code>v11</code></th>
            <td><span className={styles.previous}>Previous</span></td>
          </tr>
        </tbody>
      </table>
    </div>
  );
}

export function LandingCapabilities() {
  return (
    <section className={styles.section} id="capabilities" aria-labelledby="landing-capabilities-title">
      <h2 id="landing-capabilities-title">What you can run on Canter</h2>
      <div className={styles.grid}>
        <article className={`${styles.card} ${styles.apps}`}>
          <div className={styles.appsCopy}>
            <h3>Apps and databases</h3>
            <p>Host a web service, its Postgres database, and a public endpoint together.</p>
          </div>
          <SystemTree />
        </article>
        <article className={`${styles.card} ${styles.sandboxes}`}>
          <div>
            <h3>Sandboxes for agents</h3>
            <p>Give your agent an isolated place to run commands and test code.</p>
          </div>
          <div className={styles.command} role="group" aria-label="Example sandbox command">
            <span aria-hidden="true">&gt;</span>
            <code>go test ./...</code>
          </div>
        </article>
        <article className={`${styles.card} ${styles.ci}`}>
          <div>
            <h3>GitHub Actions runners</h3>
            <p>Run workflow jobs on Canter-managed runners.</p>
          </div>
          <BuildPipeline />
        </article>
        <article className={`${styles.card} ${styles.releases}`}>
          <div>
            <h3>Release operations</h3>
            <p>Check logs and health, or roll back to an earlier release.</p>
          </div>
          <ReleaseTable />
        </article>
      </div>
    </section>
  );
}
