"use client";

import { useEffect, useRef, useState } from "react";
import { CanterAPIError, canterFetch } from "@/lib/canter-api";
import { ciLogCollectionNotice, type CILogCollection } from "@/lib/ci";
import ci from "./ci.module.css";

type LogPage = {
  lines: { offset: number; stream: string; text: string }[];
  previous?: number;
  next?: number;
  matches: number;
  match?: number;
  matchOffset?: number;
  truncated: boolean;
};

export function CiLogReader({ workspaceId, endpoint, anchor, active, collection, githubURL, onAnchor }: {
  workspaceId: string;
  endpoint: string;
  anchor?: number;
  active: boolean;
  collection?: CILogCollection | null;
  githubURL: string | null;
  onAnchor: (offset: number) => void;
}) {
  const [page, setPage] = useState<LogPage | null>(null);
  const [input, setInput] = useState("");
  const [match, setMatch] = useState(0);
  const [offset, setOffset] = useState(0);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState(false);
  const [retry, setRetry] = useState(0);
  const [ignoreSearch, setIgnoreSearch] = useState(false);
  const scrollBox = useRef<HTMLDivElement>(null);
  const query = ignoreSearch ? "" : input;
  const queryTooLong = new TextEncoder().encode(query).length > 256;
  const collectionState = collection?.state;
  const collectionTruncated = collection?.truncated === true;

  useEffect(() => {
    const controller = new AbortController();
    if (queryTooLong) return;
    const timer = window.setTimeout(() => {
      const params = new URLSearchParams({ offset: String(anchor ?? offset) });
      if (query) {
        params.set("q", query);
        params.set("match", String(match));
      }
      void canterFetch<LogPage>(`${endpoint}?${params}`, { signal: controller.signal }).then(result => {
        if (controller.signal.aborted) return;
        setPage(result);
        setError("");
      }).catch(cause => {
        if (controller.signal.aborted) return;
        if (cause instanceof CanterAPIError && [401, 403, 404].includes(cause.status)) {
          setPage(null);
        }
        setError(cause instanceof Error ? cause.message : "Couldn’t load retained output.");
      });
    }, query ? 180 : 0);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [endpoint, anchor, offset, query, match, queryTooLong, retry, collectionState, collectionTruncated]);

  const highlighted = page?.matchOffset ?? anchor;
  useEffect(() => {
    const box = scrollBox.current;
    const row = box?.querySelector<HTMLElement>(`[data-offset="${highlighted}"]`);
    if (box && row && (row.offsetTop < box.scrollTop || row.offsetTop + row.offsetHeight > box.scrollTop + box.clientHeight)) {
      box.scrollTop = row.offsetTop;
    }
  }, [highlighted]);

  function movePage(target: number) {
    setIgnoreSearch(true);
    setOffset(target);
    onAnchor(target);
    if (scrollBox.current) scrollBox.current.scrollTop = 0;
  }

  function copyLink() {
    const url = new URL(window.location.href);
    url.searchParams.set("workspace", workspaceId);
    const line = highlighted ?? page?.lines[0]?.offset;
    if (line != null) url.searchParams.set("line", String(line));
    void navigator.clipboard.writeText(url.href).then(() => setCopied(true)).catch(() => {
      setError("Couldn’t copy the link.");
    });
  }

  const notice = ciLogCollectionNotice(collection);
  return <section className={ci.detailSection} aria-label="CI logs">
    <h3>Logs</h3>
    <div className={ci.logToolbar}>
      <input className={ci.filterInput} type="search" aria-label="Search retained output" placeholder="Search retained output…" value={input} maxLength={256} onChange={event => {
        setInput(event.target.value);
        setIgnoreSearch(false);
        setMatch(0);
        setCopied(false);
      }} />
      {query && !queryTooLong ? <span className={ci.logCount} role="status">{page?.match == null ? "No matches" : `${page.match + 1} of ${page.matches}`}</span> : null}
      <button type="button" className={ci.copyButton} disabled={!query || !page?.match} onClick={() => { setMatch(match - 1); setCopied(false); }}>Previous match</button>
      <button type="button" className={ci.copyButton} disabled={!query || page?.match == null || page.match + 1 >= page.matches} onClick={() => { setMatch(match + 1); setCopied(false); }}>Next match</button>
      <button type="button" className={ci.copyButton} onClick={copyLink}>{copied ? "Copied" : "Copy link"}</button>
    </div>
    {queryTooLong ? <p role="status" className={ci.logNotice}>Use a search of at most 256 bytes.</p> : null}
    {error ? <p role="alert" className={ci.logNotice}>{page ? "Showing previously loaded output. " : ""}{error} <button type="button" className={ci.copyButton} onClick={() => setRetry(retry + 1)}>Retry</button></p> : null}
    {collectionTruncated || page?.truncated ? <p role="status" className={ci.logNotice}>Canter retains up to 1 MiB of job output. {githubURL ? <a href={githubURL} target="_blank" rel="noreferrer">Open the job on GitHub</a> : null}</p> :
      <p role="status" className={ci.logNotice}>{notice ?? (active ? "GitHub output is collected after the job finishes." : page?.lines.length ? "Retained output available." : page ? "No retained output is available." : "Loading retained output…")}</p>}
    {page?.lines.length ? <>
      <div ref={scrollBox} className={`${ci.log} ${ci.logViewport}`} tabIndex={0} aria-label="Recorded log text">
        {page.lines.map(line => <div key={line.offset} className={ci.logLine} data-offset={line.offset} data-current={highlighted === line.offset}>
          <button type="button" className={ci.lineAnchor} aria-label={`Select ${line.stream} line at byte ${line.offset}`} onClick={() => {
            setIgnoreSearch(true);
            setCopied(false);
            onAnchor(line.offset);
          }}>{line.offset}</button><span className={ci.lineStream}>{line.stream}</span><span>{line.text.replace(/\n$/, "") || " "}</span>
        </div>)}
      </div>
      <div className={ci.logToolbar}>
        <button type="button" className={ci.copyButton} disabled={page.previous == null} onClick={() => movePage(page.previous!)}>Earlier output</button>
        <button type="button" className={ci.copyButton} disabled={page.next == null} onClick={() => movePage(page.next!)}>Later output</button>
      </div>
    </> : null}
  </section>;
}
