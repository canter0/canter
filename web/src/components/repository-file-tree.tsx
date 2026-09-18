"use client";

import { useMemo } from "react";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./repository-code.module.css";

export type RepositoryFileEntry = { path: string; additions?: number; deletions?: number; status?: string };
type TreeNode = { name: string; path: string; children: Map<string, TreeNode>; file?: RepositoryFileEntry };

export function RepositoryFileTree({ files, active, filter, onSelect }: { files: RepositoryFileEntry[]; active?: string; filter: string; onSelect: (path: string) => void }) {
  const nodes = useMemo(() => {
    const root = new Map<string, TreeNode>();
    for (const file of files) {
      const parts = file.path.split("/");
      let parent = root;
      parts.forEach((name, index) => {
        const path = parts.slice(0, index + 1).join("/");
        if (!parent.has(name)) parent.set(name, { name, path, children: new Map() });
        const node = parent.get(name)!;
        if (index === parts.length - 1) node.file = file;
        parent = node.children;
      });
    }
    return root;
  }, [files]);
  function fileButton(file: RepositoryFileEntry, label: string) {
    return <button type="button" key={file.path} className={styles.treeFile} aria-current={file.path === active ? "true" : undefined} title={file.path} onClick={() => onSelect(file.path)}><WorkspaceIcon name="file" width="14" height="14" /><span>{label}</span>{file.additions !== undefined ? <small className={styles.counts}><b>+{file.additions}</b><i>−{file.deletions}</i></small> : null}{file.status ? <span className={styles.fileStatus} data-status={file.status} aria-label={file.status}>{file.status === "added" ? "A" : file.status === "removed" ? "D" : file.status === "renamed" ? "R" : "M"}</span> : null}</button>;
  }
  function branch(children: Map<string, TreeNode>) {
    return [...children.values()].sort((a, b) => Number(!!a.file) - Number(!!b.file) || a.name.localeCompare(b.name)).map(node => node.file ? fileButton(node.file, node.name) : <details key={node.path} className={styles.treeFolder} open><summary><WorkspaceIcon name="chevron" width="12" height="12" /><WorkspaceIcon name="folder" width="14" height="14" /><span>{node.name}</span></summary><div>{branch(node.children)}</div></details>);
  }
  const matches = filter ? files.filter(file => file.path.toLowerCase().includes(filter.toLowerCase())) : files;
  return <div className={styles.fileTree}>{filter ? matches.map(file => fileButton(file, file.path)) : branch(nodes)}{!matches.length ? <p className={styles.notice}>No matching files.</p> : null}</div>;
}
