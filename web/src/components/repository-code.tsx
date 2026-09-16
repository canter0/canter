"use client";
import { useEffect, useMemo, useState } from "react";
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
import { type OperatorSurface } from "@/lib/operator-api";
import { parsePatch } from "@/lib/code-diff";
import styles from "./repository-code.module.css";

for (const [name, grammar] of Object.entries({ javascript, typescript, json, css, xml, yaml, go, python, bash })) hljs.registerLanguage(name, grammar);
function highlight(code: string, path: string) {
  const extension = path.split(".").at(-1)?.toLowerCase() ?? "";
  const language = ({ js: "javascript", jsx: "javascript", mjs: "javascript", ts: "typescript", tsx: "typescript", json: "json", css: "css", html: "xml", svg: "xml", xml: "xml", yaml: "yaml", yml: "yaml", go: "go", py: "python", sh: "bash" } as Record<string, string>)[extension];
  // highlight.js escapes all source before producing its own syntax spans.
  return language ? hljs.highlight(code, { language, ignoreIllegals: true }).value : code.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
}
type DiffFile = { filename: string; previous_filename?: string; status: string; additions: number; deletions: number; patch?: string; patchUnavailable: boolean };
type CodeResult = { content?: string; path?: string; files?: DiffFile[]; truncated?: boolean };
export function RepositoryCode({ surface, workspaceId }: { surface: OperatorSurface; workspaceId: string }) {
  const [data, setData] = useState<CodeResult | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [filename, setFilename] = useState<string | null>(null);
  const diff = surface.kind === "repository-changes";
  useEffect(() => {
    const controller = new AbortController();
    const query = new URLSearchParams({ repository: surface.repository ?? "", commit: surface.id ?? "", ...(diff ? { base: surface.base ?? "" } : { path: surface.path ?? "" }) });
    canterFetch<CodeResult>(`/workspaces/${encodeURIComponent(workspaceId)}/github/${diff ? "compare" : "file"}?${query}`, { signal: controller.signal }).then(value => { setData(value); setError(""); }).catch(cause => { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not read source."); });
    return () => controller.abort();
  }, [surface.repository, surface.id, surface.base, surface.path, workspaceId, diff, retry]);
  const file = data?.files?.find(item => item.filename === filename) ?? data?.files?.[0];
  const patch = file?.patch;
  const rows = useMemo(() => patch ? parsePatch(patch) : [], [patch]);
  const sourceHTML = useMemo(() => highlight(data?.content ?? "", surface.path ?? ""), [data?.content, surface.path]);
  const path = diff ? file?.filename : surface.path;
  const external = `https://github.com/${surface.repository}/${diff ? `compare/${surface.base}...${surface.id}` : `blob/${surface.id}/${surface.path?.split("/").map(encodeURIComponent).join("/")}`}`;
  return <section className={styles.viewer} aria-label={diff ? "Repository code changes" : "Repository source"}>
    <header className={styles.header}><div><h2>{diff ? "Changes" : path?.split("/").at(-1)}</h2><p>{surface.repository} · {diff ? `${surface.base?.slice(0, 7)} → ` : ""}{surface.id?.slice(0, 7)}</p></div><a href={external} target="_blank" rel="noreferrer">GitHub ↗</a></header>
    {error ? <p className={styles.notice} role="alert">{error} <button onClick={() => setRetry(value => value + 1)}>Retry</button></p> : null}
    {!data && !error ? <p className={styles.notice} role="status">Reading source from GitHub…</p> : null}
    {data?.truncated ? <p className={styles.notice}>This comparison is too large to show in full. Open GitHub for the complete comparison.</p> : null}
    {data && diff && !data.files?.length ? <p className={styles.notice}>No file changes between these commits.</p> : null}
    {data?.files?.length ? <nav className={styles.files} aria-label="Changed files">{data.files.map(item => <button key={item.filename} aria-current={item.filename === file?.filename ? "true" : undefined} onClick={() => setFilename(item.filename)}><span>{item.filename}</span><small><b>+{item.additions}</b> <i>−{item.deletions}</i></small></button>)}</nav> : null}
    {data && path ? <div className={styles.document}><div className={styles.fileHeader}><span>{path}</span>{file ? <small>{file.status}{file.previous_filename ? ` from ${file.previous_filename}` : ""}</small> : null}</div>
      {diff ? file?.patch ? <div className={styles.codeScroll}><table className={styles.diff} aria-label={`Code changes in ${path}`}><tbody>{rows.map((row, index) => <tr key={index} data-kind={row.kind}><td className={styles.number}>{row.before}</td><td className={styles.number}>{row.after}</td><td className={styles.sign}>{row.kind === "added" ? "+" : row.kind === "removed" ? "−" : ""}</td><td><code dangerouslySetInnerHTML={{ __html: highlight(row.text, path) }} /></td></tr>)}</tbody></table></div> : <p className={styles.notice}>GitHub did not provide a text patch for this file. It may be binary or too large. <a href={external} target="_blank" rel="noreferrer">View on GitHub</a></p> : <div className={`${styles.codeScroll} ${styles.source}`}><pre className={styles.gutter} aria-hidden>{data.content?.split("\n").map((_, index) => index + 1).join("\n")}</pre><pre><code dangerouslySetInnerHTML={{ __html: sourceHTML }} /></pre></div>}
    </div> : null}
  </section>;
}
