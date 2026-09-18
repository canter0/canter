"use client";
import { useEffect, useMemo, useRef, useState } from "react";
import hljs from "highlight.js/lib/core";
import javascript from "highlight.js/lib/languages/javascript";
import typescript from "highlight.js/lib/languages/typescript";
import json from "highlight.js/lib/languages/json";
import css from "highlight.js/lib/languages/css";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";
import go from "highlight.js/lib/languages/go";
import python from "highlight.js/lib/languages/python";
import bash from "highlight.js/lib/languages/bash";
import { canterFetch } from "@/lib/canter-api";
import { inspectRepository } from "@/lib/repository-api";
import { type OperatorSurface } from "@/lib/operator-api";
import { parsePatch, splitPatch } from "@/lib/code-diff";
import { WorkspaceIcon } from "./workspace-icon";
import { RepositoryFileTree } from "./repository-file-tree";
import styles from "./repository-code.module.css";

for (const [name, grammar] of Object.entries({ javascript, typescript, json, css, xml, yaml, go, python, bash })) hljs.registerLanguage(name, grammar);
function highlight(code: string, path: string) {
  const extension = path.split(".").at(-1)?.toLowerCase() ?? "";
  const language = ({ js: "javascript", jsx: "javascript", mjs: "javascript", ts: "typescript", tsx: "typescript", json: "json", css: "css", html: "xml", svg: "xml", xml: "xml", yaml: "yaml", yml: "yaml", go: "go", py: "python", sh: "bash" } as Record<string, string>)[extension];
  // highlight.js escapes source before producing its own syntax spans.
  return language ? hljs.highlight(code, { language, ignoreIllegals: true }).value : code.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
}
function highlightSourceLines(code: string, path: string) {
  const open: string[] = [];
  return highlight(code, path).split("\n").map(line => {
    const prefix = open.join("");
    for (const tag of line.matchAll(/<span\b[^>]*>|<\/span>/g)) {
      if (tag[0] === "</span>") open.pop(); else open.push(tag[0]);
    }
    return prefix + line + "</span>".repeat(open.length);
  });
}
type DiffFile = { filename: string; previous_filename?: string; status: string; additions: number; deletions: number; patch?: string; patchUnavailable: boolean };
type CodeResult = { content?: string; path?: string; files?: DiffFile[]; truncated?: boolean };
type RepositoryTree = { files: string[]; truncated: boolean };

function Patch({ file, split }: { file: DiffFile; split: boolean }) {
  const rows = useMemo(() => parsePatch(file.patch ?? ""), [file.patch]);
  const columns = useMemo(() => splitPatch(rows), [rows]);
  return <table className={styles.diff} data-split={split} aria-label={`Code changes in ${file.filename}`}><colgroup><col className={styles.numberColumn} />{split ? <><col /><col className={styles.numberColumn} /><col /></> : <><col className={styles.numberColumn} /><col className={styles.signColumn} /><col /></>}</colgroup><tbody>{split ? columns.map((row, index) => row.kind === "hunk" ? <tr key={index} data-kind="hunk"><td colSpan={4}><code>{row.text}</code></td></tr> : <tr key={index}>
    <td className={styles.number} data-kind={row.before?.kind}>{row.before?.before}</td><td className={styles.splitCell} data-kind={row.before?.kind}><code dangerouslySetInnerHTML={{ __html: highlight(row.before?.text ?? "", file.filename) }} /></td>
    <td className={styles.number} data-kind={row.after?.kind}>{row.after?.after}</td><td className={styles.splitCell} data-kind={row.after?.kind}><code dangerouslySetInnerHTML={{ __html: highlight(row.after?.text ?? "", file.filename) }} /></td>
  </tr>) : rows.map((row, index) => <tr key={index} data-kind={row.kind}><td className={styles.number}>{row.before}</td><td className={styles.number}>{row.after}</td><td className={styles.sign}>{row.kind === "added" ? "+" : row.kind === "removed" ? "−" : ""}</td><td><code dangerouslySetInnerHTML={{ __html: highlight(row.text, file.filename) }} /></td></tr>)}</tbody></table>;
}

export function RepositoryCode({ surface, workspaceId, onSelect }: { surface: OperatorSurface; workspaceId: string; onSelect?: (surface: OperatorSurface) => void }) {
  const [data, setData] = useState<CodeResult | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [filename, setFilename] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [showTree, setShowTree] = useState(true);
  const [split, setSplit] = useState(false);
  const [wrap, setWrap] = useState(false);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [tree, setTree] = useState<RepositoryTree | null>(null);
  const [treeError, setTreeError] = useState("");
  const [copyStatus, setCopyStatus] = useState("");
  const [copiedPath, setCopiedPath] = useState("");
  const documents = useRef(new Map<string, HTMLElement>());
  const diff = surface.kind === "repository-changes";
  const canNavigate = !!onSelect;
  useEffect(() => {
    const controller = new AbortController();
    const query = new URLSearchParams({ repository: surface.repository ?? "", commit: surface.id ?? "", ...(diff ? { base: surface.base ?? "" } : { path: surface.path ?? "" }) });
    canterFetch<CodeResult>(`/workspaces/${encodeURIComponent(workspaceId)}/github/${diff ? "compare" : "file"}?${query}`, { signal: controller.signal }).then(value => { if (!controller.signal.aborted) { setData(value); setError(""); } }).catch(cause => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not read source."); });
    return () => controller.abort();
  }, [surface.repository, surface.id, surface.base, surface.path, workspaceId, diff, retry]);
  useEffect(() => {
    if (diff || !canNavigate) return;
    const controller = new AbortController();
    inspectRepository(workspaceId, surface.repository ?? "", surface.id ?? "", retry > 0).then(value => { if (!controller.signal.aborted) { setTree(value); setTreeError(""); } }).catch(cause => { if (!controller.signal.aborted) setTreeError(cause instanceof Error ? cause.message : "Could not read files."); });
    return () => controller.abort();
  }, [surface.repository, surface.id, workspaceId, diff, canNavigate, retry]);
  useEffect(() => { if (!copiedPath) return; const timer = setTimeout(() => setCopiedPath(""), 2200); return () => clearTimeout(timer); }, [copiedPath]);
  const sourceLines = useMemo(() => highlightSourceLines(data?.content ?? "", surface.path ?? ""), [data?.content, surface.path]);
  const files = data?.files ?? [];
  const allCollapsed = !!files.length && files.every(file => collapsed.has(file.filename));
  const totals = files.reduce((sum, file) => ({ additions: sum.additions + file.additions, deletions: sum.deletions + file.deletions }), { additions: 0, deletions: 0 });
  const treeFiles = useMemo(() => diff ? (data?.files ?? []).map(file => ({ path: file.filename, additions: file.additions, deletions: file.deletions, status: file.status })) : (tree?.files ?? []).map(path => ({ path })), [diff, data?.files, tree?.files]);
  const external = `https://github.com/${surface.repository}/${diff ? `compare/${surface.base}...${surface.id}` : `blob/${surface.id}/${surface.path?.split("/").map(encodeURIComponent).join("/")}`}`;
  const toggle = (path: string) => setCollapsed(current => { const next = new Set(current); if (next.has(path)) next.delete(path); else next.add(path); return next; });
  function selectFile(path: string) {
    if (!diff) { onSelect?.({ kind: "file", repository: surface.repository, id: surface.id, path }); return; }
    setFilename(path); setCollapsed(current => { const next = new Set(current); next.delete(path); return next; });
    requestAnimationFrame(() => documents.current.get(path)?.scrollIntoView({ block: "start", behavior: "instant" }));
  }
  async function copy(path: string) {
    try { await navigator.clipboard.writeText(path); setCopiedPath(path); setCopyStatus("File path copied."); }
    catch { setCopyStatus("Could not copy. Select the file path to copy it manually."); }
  }
  return <section className={styles.viewer} aria-label={diff ? "Repository code changes" : "Repository source"}>
    <header className={styles.header}><div className={styles.repositoryIdentity}><WorkspaceIcon name="folder" width="15" height="15" /><span title={surface.repository}>{surface.repository}</span></div><a href={external} target="_blank" rel="noreferrer" title="Open on GitHub">GitHub <WorkspaceIcon name="external" width="14" height="14" /></a></header>
    <div className={styles.reviewToolbar}>
      <div className={styles.revision}><WorkspaceIcon name="activity" width="14" height="14" /><span title={diff ? `${surface.base} → ${surface.id}` : surface.id}>{diff ? `${surface.base?.slice(0, 7)} → ` : ""}{surface.id?.slice(0, 7)}</span>{diff && data ? <><span className={styles.fileCount}>{files.length}{data.truncated ? "+" : ""} files</span><span className={styles.counts}><b>+{totals.additions}</b><i>−{totals.deletions}</i></span></> : null}</div>
      <div className={styles.reviewActions}>{diff ? <><select aria-label="Diff layout" value={split ? "split" : "unified"} onChange={event => setSplit(event.target.value === "split")}><option value="unified">Unified</option><option value="split">Split</option></select><button type="button" title={allCollapsed ? "Expand all files" : "Collapse all files"} aria-label={allCollapsed ? "Expand all files" : "Collapse all files"} disabled={!files.length} onClick={() => setCollapsed(allCollapsed ? new Set() : new Set(files.map(file => file.filename)))}><WorkspaceIcon name={allCollapsed ? "plus" : "down"} width="15" height="15" /></button></> : null}<button type="button" title="Wrap lines" aria-label="Wrap lines" aria-pressed={wrap} onClick={() => setWrap(!wrap)} className={styles.wrapButton}>↵</button>{diff || onSelect ? <button type="button" title={showTree ? "Hide file tree" : "Show file tree"} aria-label={showTree ? "Hide file tree" : "Show file tree"} aria-pressed={showTree} onClick={() => setShowTree(!showTree)}><WorkspaceIcon name="panel" width="15" height="15" /></button> : null}</div>
    </div>
    <div className={styles.reviewLayout} data-tree={showTree && (diff || !!onSelect)}>
      <div className={styles.documents} data-wrap={wrap}>
        {error ? <p className={styles.notice} role="alert">{error} <button onClick={() => setRetry(value => value + 1)}>Retry</button></p> : null}
        {!data && !error ? <p className={styles.notice} role="status">Reading source from GitHub…</p> : null}
        {data?.truncated ? <p className={styles.notice}>This comparison is too large to show in full. <a href={external} target="_blank" rel="noreferrer">View the full comparison on GitHub</a>.</p> : null}
        {data && diff && !files.length ? <div className={styles.emptyState}><WorkspaceIcon name="check" width="24" height="24" /><p>No changes between these commits.</p></div> : null}
        {diff ? files.map(file => <article key={file.filename} ref={element => { if (element) documents.current.set(file.filename, element); else documents.current.delete(file.filename); }} className={styles.document} data-selected={file.filename === filename}>
          <header className={styles.fileHeader}><button type="button" className={styles.fileToggle} aria-label={`${collapsed.has(file.filename) ? "Expand" : "Collapse"} ${file.filename}`} aria-expanded={!collapsed.has(file.filename)} onClick={() => toggle(file.filename)}><WorkspaceIcon name={collapsed.has(file.filename) ? "chevron" : "down"} width="14" height="14" /><WorkspaceIcon name="file" width="14" height="14" /><span title={file.filename}>{file.filename}</span></button><span className={styles.counts}><b>+{file.additions}</b><i>−{file.deletions}</i></span><button type="button" className={styles.copyButton} aria-label={`Copy path ${file.filename}`} title="Copy file path" onClick={() => void copy(file.filename)}><WorkspaceIcon name={copiedPath === file.filename ? "check" : "copy"} width="14" height="14" /></button></header>
          {!collapsed.has(file.filename) ? <>{file.previous_filename ? <p className={styles.renameNote}>Renamed from {file.previous_filename}</p> : null}{file.patch ? <div className={styles.codeScroll}><Patch file={file} split={split} /></div> : <p className={styles.notice}>No text patch is available for this {file.status} file. It may be binary or too large. <a href={external} target="_blank" rel="noreferrer">View on GitHub</a></p>}</> : null}
        </article>) : data && surface.path ? <article className={styles.document}><header className={styles.fileHeader}><WorkspaceIcon name="file" width="14" height="14" /><span className={styles.sourcePath}>{surface.path}</span><small>{data.content?.split("\n").length ?? 0} lines</small><button type="button" className={styles.copyButton} aria-label={`Copy path ${surface.path}`} title="Copy file path" onClick={() => void copy(surface.path!)}><WorkspaceIcon name={copiedPath === surface.path ? "check" : "copy"} width="14" height="14" /></button></header><div className={styles.codeScroll}><table className={`${styles.diff} ${styles.sourceTable}`} aria-label={`Source for ${surface.path}`}><tbody>{sourceLines.map((line, index) => <tr key={index}><td className={styles.number}>{index + 1}</td><td><code dangerouslySetInnerHTML={{ __html: line || " " }} /></td></tr>)}</tbody></table></div></article> : null}
      </div>
      {showTree && (diff || onSelect) ? <nav className={styles.treePanel} aria-label={diff ? "Changed files" : "Repository files"}><div className={styles.treeHeading}><span>{diff ? "Changed files" : "Files"}</span>{!diff && onSelect ? <button type="button" title="Repository overview" aria-label="Repository overview" onClick={() => onSelect({ kind: "repository", repository: surface.repository, id: surface.id })}><WorkspaceIcon name="folder" width="14" height="14" /></button> : <small>{treeFiles.length}</small>}</div><label className={styles.treeSearch}><WorkspaceIcon name="search" width="14" height="14" /><input aria-label={diff ? "Search changed files" : "Search repository files"} placeholder="Search files…" value={filter} onChange={event => setFilter(event.target.value)} />{filter ? <button type="button" aria-label="Clear file search" onClick={() => setFilter("")}><WorkspaceIcon name="close" width="12" height="12" /></button> : null}</label>{treeError && !diff ? <p className={styles.notice} role="alert">{treeError} <button onClick={() => setRetry(value => value + 1)}>Retry</button></p> : !diff && !tree ? <p className={styles.notice} role="status">Loading files…</p> : <RepositoryFileTree files={treeFiles} active={diff ? filename ?? files[0]?.filename : surface.path} filter={filter} onSelect={selectFile} />}{!diff && tree?.truncated ? <p className={styles.notice}>Showing the first {tree.files.length} files.</p> : null}</nav> : null}
    </div>
    <span role="status" className="sr-only">{copyStatus}</span>
  </section>;
}
