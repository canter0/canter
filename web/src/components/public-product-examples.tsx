import type { ReactNode } from "react";
import type { PublicProductId } from "@/lib/public-products";
import { SystemTree } from "./landing-capabilities";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import styles from "./public-product.module.css";

type Item = readonly [icon: WorkspaceIconName, label: string];

function Mark({ item }: { item: Item }) {
  return <><WorkspaceIcon name={item[0]} width={20} height={20} /><span>{item[1]}</span></>;
}

function Flow({ label, steps }: { label: string; steps: readonly Item[] }) {
  return (
    <ol className={styles.flow} aria-label={label}>
      {steps.map(step => <li key={step[1]}><Mark item={step} /></li>)}
    </ol>
  );
}

function Example({ label, title, children }: { label: string; title?: ReactNode; children: ReactNode }) {
  return (
    <div className={styles.example} role="group" aria-label={label}>
      {title ? <div className={styles.exampleTitle}>{title}</div> : null}
      {children}
    </div>
  );
}

function Status({ children, active = false }: { children: string; active?: boolean }) {
  return <span className={styles.status} data-active={active}>
    <WorkspaceIcon name={active ? "clock" : "check"} width={14} height={14} />{children}
  </span>;
}

function ExampleTable({ label, columns, rows }: { label: string; columns: readonly string[]; rows: readonly (readonly ReactNode[])[] }) {
  return (
    <Example label={label}>
      <table className={styles.table}>
        <thead><tr>{columns.map(column => <th key={column} scope="col">{column}</th>)}</tr></thead>
        <tbody>{rows.map(([name, ...cells], row) => (
          <tr key={row}>
            <th scope="row">{name}</th>
            {cells.map((cell, column) => <td key={column}>{cell}</td>)}
          </tr>
        ))}</tbody>
      </table>
    </Example>
  );
}

function ReleaseExample({ showRollback = false }: { showRollback?: boolean }) {
  return (
    <Example label="Example release for team-board" title={<Mark item={["apps", "team-board"]} />}>
      <div className={styles.exampleRow}><code>v12</code><Status>Live</Status></div>
      <ul className={styles.exampleTabs}>
        <li><Mark item={["activity", "Health checks"]} /></li>
        <li><Mark item={["file", "Logs"]} /></li>
        {showRollback ? <li><Mark item={["reply", "Roll back"]} /></li> : null}
      </ul>
    </Example>
  );
}

const recentJobs = [
  ["Build", <Status key="build">Passed</Status>],
  ["Test", <Status key="test">Passed</Status>],
];

export const productExamples = {
  computers: [
    <Example key="files" label="Example file browser" title={<Mark item={["desktop", "dev-box"]} />}>
      <div className={styles.fileBrowser}>
        <div className={styles.fileSidebar}><Mark item={["folder", "Files"]} /></div>
        <ul className={styles.fileList}>
          <li><Mark item={["folder", "src"]} /></li>
          <li><Mark item={["file", "README.md"]} /></li>
        </ul>
      </div>
    </Example>,
    <Example key="jobs" label="Example sandbox job">
      <ul className={styles.exampleTabs}><li>Files</li><li className={styles.activeTab}><Mark item={["terminal", "Jobs"]} /></li></ul>
      <div className={styles.exampleRow}><code>go test ./...</code><Status>Passed</Status></div>
    </Example>,
    <Flow key="wake" label="Example wake sequence" steps={[["clock", "Asleep"], ["desktop", "Running"]]} />,
  ],
  "super-sandboxes": [
    <div key="batch" className={styles.batch} role="group" aria-label="Example batch distributed to three workers">
      <div className={styles.batchSource}><Mark item={["sandboxCollection", "Batch"]} /></div>
      <ul className={styles.workers}>
        {["Worker A", "Worker B", "Worker C"].map(worker => <li key={worker}><Mark item={["sandbox", worker]} /></li>)}
      </ul>
    </div>,
    <ExampleTable key="tasks" label="Example batch tasks" columns={["Task", "Worker", "State"]} rows={[
      ["Unit tests", "Worker A", <Status key="unit">Succeeded</Status>],
      ["Lint", "Worker B", <Status key="lint" active>Running</Status>],
    ]} />,
    <Flow key="logs" label="Find a task’s logs" steps={[["file", "Task"], ["sandbox", "Worker"], ["terminal", "Jobs"]]} />,
  ],
  "saved-environments": [
    <Example key="saved" label="Example saved environment">
      <div className={styles.exampleRow}><div className={styles.mark}><Mark item={["bookmark", "project-dev"]} /></div><Status>Current save</Status></div>
    </Example>,
    <Flow key="save" label="Save a sandbox environment" steps={[["desktop", "Running sandbox"], ["clock", "Sleep"], ["bookmark", "Saved environment"]]} />,
    <Example key="templates" label="Example template" title="Templates">
      <div className={styles.exampleRow}><div className={styles.mark}><Mark item={["terminal", "Go development"]} /></div></div>
    </Example>,
  ],
  vps: [
    <Flow key="topology" label="Example app architecture" steps={[["globe", "HTTPS endpoint"], ["serverStack", "Web service"], ["stack", "Postgres"]]} />,
    <Flow key="approval" label="Deployment approval sequence" steps={[["file", "Agent drafts"], ["check", "You approve"], ["cloudServer", "Canter deploys"]]} />,
    <ReleaseExample key="release" showRollback />,
  ],
  apps: [
    <ExampleTable key="inventory" label="Example app inventory" columns={["Apps", "Status"]} rows={[
      ["team-board", <Status key="board">Running</Status>],
      ["docs-site", <Status key="docs">Running</Status>],
    ]} />,
    <SystemTree key="resources" name="team-board" />,
    <ReleaseExample key="release" />,
  ],
  ci: [
    <Example key="pipeline" label="Example GitHub Actions workflow">
      <div className={styles.pipeline}>
        <Flow label="Example workflow jobs" steps={[["gear", "Build"], ["check", "Test"]]} />
        <div className={styles.runner}><Mark item={["serverStack", "Canter runner"]} /></div>
      </div>
    </Example>,
    <Example key="workflow" label="Example workflow configuration" title={<Mark item={["file", "workflow.yml"]} />}>
      <pre className={styles.codeExample}><code>{"jobs:\n  build:\n"}<span>{"    runs-on: <runner label>"}</span></code></pre>
    </Example>,
    <Flow key="setup" label="Connect GitHub and run a job" steps={[["changes", "Connect GitHub"], ["serverStack", "Choose a runner"], ["terminal", "Run a job"]]} />,
  ],
  "ci-workspace": [
    <ExampleTable key="recent" label="Example recent jobs" columns={["Recent jobs", "Status"]} rows={recentJobs} />,
    <Example key="filter" label="Example filtered jobs">
      <div className={styles.searchExample}><Mark item={["search", "Filter jobs…"]} /></div>
      <div className={styles.exampleRow}><span>Unit tests</span><div className={styles.mark}><Mark item={["external", "GitHub"]} /></div></div>
    </Example>,
    <Example key="runner-pool" label="Example runner pool">
      <div className={styles.exampleRow}><strong>Runner pool</strong><Status>Ready</Status></div>
      <div className={styles.exampleRow}><strong>Runner sizes</strong><div className={styles.mark}><Mark item={["copy", "Copy runner label"]} /></div></div>
    </Example>,
  ],
} satisfies Record<PublicProductId, readonly [ReactNode, ReactNode, ReactNode]>;
