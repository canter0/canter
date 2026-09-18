"use client";
import { useEffect, useMemo, useState } from "react";
import { inspectRepository, type RepositoryInspection } from "@/lib/repository-api";
import type { OperatorSurface } from "@/lib/operator-api";
import { ProviderIcon } from "./provider-icon";
import { WorkspaceIcon } from "./workspace-icon";
import { RepositoryFileTree } from "./repository-file-tree";
import styles from "./repository-overview.module.css";

export function RepositoryOverview({ surface, workspaceId, onSelect }: { surface: OperatorSurface; workspaceId: string; onSelect: (surface: OperatorSurface) => void }) {
  const [data, setData] = useState<RepositoryInspection | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [filter, setFilter] = useState("");
  const [asTree, setAsTree] = useState(true);
  useEffect(() => {
    const controller = new AbortController();
    inspectRepository(workspaceId, surface.repository ?? "", surface.id ?? "", retry > 0).then(value => { if (!controller.signal.aborted) { setData(value); setError(""); } }).catch(cause => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not read repository."); });
    return () => controller.abort();
  }, [surface.repository, surface.id, workspaceId, retry]);
  const files = data?.files;
  const treeFiles = useMemo(() => (files ?? []).map(path => ({ path })), [files]);
  const manifest = files?.find(path => ["package.json", "go.mod", "Cargo.toml", "pyproject.toml", "requirements.txt"].includes(path));
  const staticRoot = files?.includes("index.html");
  const readme = files?.find(path => /^readme(?:\.md|\.txt|\.rst)?$/i.test(path));
  const openFile = (path: string) => { if (data) onSelect({ kind: "file", repository: surface.repository, id: data.commit, path }); };
  const matching = (files ?? []).filter(path => path.toLowerCase().includes(filter.toLowerCase()));
  return <section className={styles.overview} aria-label="Repository overview">
    <header className={styles.header}><div className={styles.identity}><ProviderIcon provider="github" /><span>{surface.repository}</span></div><a href={`https://github.com/${surface.repository}`} target="_blank" rel="noreferrer" aria-label="Open repository on GitHub" title="Open on GitHub"><WorkspaceIcon name="external" width="16" height="16" /></a></header>
    <div className={styles.summary}><div className={styles.titleRow}><h2>{surface.repository?.split("/").slice(1).join("/")}</h2>{data ? <span className={styles.visibility}>{data.private ? "Private" : "Public"}</span> : null}</div>{data?.description ? <p className={styles.description}>{data.description}</p> : null}{data ? <div className={styles.metadata}><span><WorkspaceIcon name="activity" width="13" height="13" />{data.defaultBranch}</span><code title={data.commit}>{data.commit.slice(0, 7)}</code>{data.language ? <span className={styles.language}>{data.language}</span> : null}</div> : null}</div>
    {error ? <p className={styles.note} role="alert">{error} <button onClick={() => setRetry(value => value + 1)}>Retry</button></p> : null}
    {!data && !error ? <p className={styles.note} role="status">Reading repository…</p> : null}
    {data ? <>
      <div className={styles.actions}>{readme ? <button type="button" onClick={() => openFile(readme)}><WorkspaceIcon name="file" width="14" height="14" />Readme<WorkspaceIcon name="chevron" width="12" height="12" /></button> : null}{data.parent ? <button type="button" onClick={() => onSelect({ kind: "repository-changes", repository: surface.repository, id: data.commit, base: data.parent })}><WorkspaceIcon name="activity" width="14" height="14" />Latest changes<WorkspaceIcon name="chevron" width="12" height="12" /></button> : null}</div>
      <section className={styles.files} aria-label="Repository files"><div className={styles.fileHeading}><h3>Files <span>{files?.length}{data.truncated ? "+" : ""}</span></h3><button type="button" aria-label={asTree ? "View files as list" : "View files as tree"} title={asTree ? "View as list" : "View as tree"} onClick={() => setAsTree(!asTree)}><WorkspaceIcon name={asTree ? "file" : "folder"} width="15" height="15" /></button></div>
        <label className={styles.search}><WorkspaceIcon name="search" width="15" height="15" /><input aria-label="Find a repository file" placeholder="Search files…" autoComplete="off" spellCheck={false} value={filter} onChange={event => setFilter(event.target.value)} />{filter ? <button type="button" aria-label="Clear file search" onClick={() => setFilter("")}><WorkspaceIcon name="close" width="13" height="13" /></button> : <kbd>⌕</kbd>}</label>
        {asTree ? <RepositoryFileTree files={treeFiles} filter={filter} onSelect={openFile} /> : <div className={styles.fileList}>{matching.map(path => <button type="button" key={path} onClick={() => openFile(path)}><WorkspaceIcon name="file" width="14" height="14" /><span>{path}</span><WorkspaceIcon name="chevron" width="12" height="12" /></button>)}{!matching.length ? <p className={styles.note}>No matching files.</p> : null}</div>}
        {data.truncated ? <p className={styles.note}>Showing the first {files?.length} files. GitHub has the complete tree.</p> : null}
      </section>
      <div className={styles.runtime}><WorkspaceIcon name={staticRoot ? "apps" : "terminal"} width="16" height="16" /><div><strong>{staticRoot ? "Static entry point" : manifest ? "Build configuration" : "Source repository"}</strong><p>{staticRoot ? "index.html at the repository root" : manifest ?? "Open a file to inspect the source."}</p></div>{manifest ? <button type="button" onClick={() => openFile(manifest)} aria-label={`Read ${manifest}`}><WorkspaceIcon name="chevron" width="14" height="14" /></button> : null}</div>
      <p className={styles.footer}><WorkspaceIcon name="check" width="12" height="12" />Viewing source at {data.commit.slice(0, 7)}</p>
    </> : null}
  </section>;
}
