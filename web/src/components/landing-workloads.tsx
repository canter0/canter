import { BuildPipeline, SystemTree } from "./landing-capabilities";
import { LandingWorkloadIndex } from "./landing-workload-index";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./landing-workloads.module.css";

const workloads = [
  {
    id: "workload-vps",
    label: "VPS",
    title: "Run your app on a VPS.",
    description: "Host the app, its database, and a public HTTPS endpoint.",
    preview: <SystemTree name="team-board" className={styles.system} />,
  },
  {
    id: "workload-sandboxes",
    label: "Sandboxes",
    title: "Try a change in isolation.",
    description: "Give your agent a sandbox for commands and tests.",
    preview: (
      <div className={styles.sandbox} role="group" aria-label="Example sandbox command and result">
        <div className={styles.command}>
          <span aria-hidden="true">&gt;</span>
          <code>go test ./...</code>
        </div>
        <div className={styles.result}>
          <span><WorkspaceIcon name="check" width={14} height={14} /></span>
          Tests passed
        </div>
      </div>
    ),
  },
  {
    id: "workload-ci",
    label: "CI",
    title: "Build with GitHub Actions.",
    description: "Run your workflow jobs on Canter-managed runners.",
    preview: (
      <div className={styles.ci} role="group" aria-label="Example GitHub Actions run">
        <BuildPipeline />
        <div className={styles.ciDetails}>
          <span className={styles.passed}>
            <WorkspaceIcon name="check" width={14} height={14} />
            Passed
          </span>
          <span className={styles.runner}>
            <WorkspaceIcon name="serverStack" />
            Canter runner
          </span>
        </div>
      </div>
    ),
  },
];

const chapters = workloads.map(({ id, label }) => ({ id, label }));

export function LandingWorkloads() {
  return (
    <section className={styles.section} id="workloads" aria-labelledby="workloads-title">
      <h2 id="workloads-title">A place for every workload</h2>
      <div className={styles.layout}>
        <LandingWorkloadIndex chapters={chapters} />
        <div className={styles.rows}>
          {workloads.map(({ id, label, title, description, preview }) => (
            <article key={id} id={id} className={styles.row} aria-labelledby={`${id}-title`} tabIndex={-1}>
              <div className={styles.copy}>
                <p className={styles.mobileLabel}>{label}</p>
                <h3 id={`${id}-title`}>{title}</h3>
                <p className={styles.description}>{description}</p>
              </div>
              <div className={styles.preview}>{preview}</div>
            </article>
          ))}
        </div>
      </div>
    </section>
  );
}
