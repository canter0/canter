"use client";
import { useEffect, useRef, useState, type RefObject } from "react";
import { canterFetch } from "@/lib/canter-api";
import { type Conversation, type OperatorSurface } from "@/lib/operator-api";
import { githubConnectURL, type GitHubConnection } from "@/lib/github-connection";
import { ProviderIcon } from "./provider-icon";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import styles from "./github-mention-picker.module.css";

type Result = { connection: GitHubConnection; repositories: { full_name: string; description?: string; private: boolean }[]; page: number; hasMore: boolean };
type Choice = { label: string; detail: string; icon: WorkspaceIconName; token: string; surface: OperatorSurface };
type Props = { workspaceId?: string; query: string; category: string | null; systems: string[]; deployments: { id: string; system: string; summary: string }[]; conversations: Conversation[]; inputRef: RefObject<HTMLTextAreaElement | null>; onPick: (choice: Choice) => void; onClose: () => void };
const categories: Choice[] = [
  { label: "GitHub repositories", detail: "Repositories and source code", icon: "folder", token: "github", surface: { kind: "github" } },
  { label: "Apps", detail: "Your workspace apps", icon: "apps", token: "apps", surface: { kind: "apps" } },
  { label: "Deployments", detail: "Proposals and releases", icon: "activity", token: "deployments", surface: { kind: "deployments" } },
  { label: "Billing", detail: "Usage and payment", icon: "file", token: "billing", surface: { kind: "billing" } },
];
export function GitHubMentionPicker({ workspaceId, query, category, systems, deployments, conversations, inputRef, onPick, onClose }: Props) {
  const [data, setData] = useState<Result | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [moreLoading, setMoreLoading] = useState(false);
  const panel = useRef<HTMLDivElement>(null);
  const normalized = query.trim().toLowerCase();
  const section = category ?? (["github", "apps", "deployments", "conversations"].find(value => value === normalized) ?? null);
  const search = category ? normalized : section ? "" : normalized;
  useEffect(() => {
    if (!workspaceId) return;
    const controller = new AbortController();
    canterFetch<Result>(`/workspaces/${encodeURIComponent(workspaceId)}/github/repositories`, { signal: controller.signal }).then(value => { setData(value); setError(""); }).catch(() => { if (!controller.signal.aborted) setError("Could not load repositories."); });
    return () => controller.abort();
  }, [workspaceId, attempt]);
  async function loadMore() {
    if (!data || moreLoading || !workspaceId) return;
    setMoreLoading(true); setError("");
    try {
      const next = await canterFetch<Result>(`/workspaces/${encodeURIComponent(workspaceId)}/github/repositories?page=${data.page + 1}`);
      setData({ ...next, repositories: next.connection.connected ? [...new Map([...data.repositories, ...next.repositories].map(repo => [repo.full_name, repo])).values()] : [] });
    } catch { setError("Could not load more repositories."); }
    finally { setMoreLoading(false); }
  }
  const appChoices: Choice[] = systems.map(name => ({ label: name, detail: "App", icon: "apps", token: name, surface: { kind: "app", system: name } }));
  const deploymentChoices: Choice[] = deployments.map(item => ({ label: item.system, detail: item.summary, icon: "activity", token: item.system, surface: { kind: "deployment", id: item.id, system: item.system } }));
  const conversationChoices: Choice[] = conversations.map(item => ({ label: item.title, detail: "Conversation", icon: "message", token: item.title, surface: { kind: "conversation", id: item.id } }));
  const repositoryChoices: Choice[] = (data?.repositories ?? []).map(repo => ({ label: repo.full_name, detail: repo.description || (repo.private ? "Private repository" : "Public repository"), icon: "folder", token: repo.full_name, surface: { kind: "repository", repository: repo.full_name } }));
  const visible = (section === "github" ? repositoryChoices : section === "apps" ? appChoices : section === "deployments" ? deploymentChoices : section === "conversations" ? conversationChoices : [...categories, ...appChoices, ...deploymentChoices, ...conversationChoices, ...repositoryChoices])
    .filter(item => !search || `${item.label} ${item.detail}`.toLowerCase().includes(search))
    .sort((left, right) => {
      if (!search) return 0;
      const rank = (item: Choice) => item.label.toLowerCase().startsWith(search) ? 0 : item.label.toLowerCase().includes(search) ? 1 : 2;
      return rank(left) - rank(right);
    });
  const reminder = data && !data.connection.connected && ((!section && !normalized) || section === "github") ? <div className={styles.connect}><strong>Connect GitHub</strong><p>Connect your account to find and attach repositories.</p>{data.connection.enabled && workspaceId ? <a href={githubConnectURL(data.connection, workspaceId, typeof window === "undefined" ? "/app" : window.location.pathname)}>Connect GitHub ↗</a> : <p>GitHub access is not configured on this server.</p>}<small>Choose repositories for Canter to read and deploy. GitHub will show the permissions before you connect.</small></div> : null;
  useEffect(() => {
    const input = inputRef.current;
    const keyboard = (event: KeyboardEvent) => {
      if (event.isComposing) return;
      if (event.key === "Escape") { event.preventDefault(); onClose(); return; }
      if (event.key === "ArrowDown") { event.preventDefault(); panel.current?.querySelector<HTMLElement>("[data-pick]")?.focus(); }
      if (event.key === "Tab" || (event.key === "Enter" && !event.shiftKey)) {
        const first = panel.current?.querySelector<HTMLElement>("[data-pick]");
        if (first) { event.preventDefault(); first.click(); }
      }
    };
    const outside = (event: PointerEvent) => { if (event.target instanceof Node && !panel.current?.contains(event.target) && event.target !== input) onClose(); };
    input?.addEventListener("keydown", keyboard); document.addEventListener("pointerdown", outside);
    return () => { input?.removeEventListener("keydown", keyboard); document.removeEventListener("pointerdown", outside); };
  }, [inputRef, onClose]);
  return <div ref={panel} id="context-mention-picker" className={styles.panel} role="dialog" aria-label="Insert workspace context" onKeyDown={event => {
    if (event.key === "Escape") { event.preventDefault(); onClose(); inputRef.current?.focus(); }
    if (["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault(); const items = Array.from(panel.current?.querySelectorAll<HTMLElement>("[data-pick], a") ?? []);
      const index = items.indexOf(document.activeElement as HTMLElement); items[(index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length]?.focus();
    }
  }}>
    {section ? <div className={styles.section}>{section === "github" ? <ProviderIcon provider="github" /> : <WorkspaceIcon name={section === "apps" ? "apps" : section === "deployments" ? "activity" : "message"} />}<strong>{section === "github" ? "GitHub" : section === "conversations" ? "Conversations" : section === "apps" ? "Apps" : "Deployments"}</strong><span>{section === "github" ? "Repositories and source code" : section === "conversations" ? "Earlier conversations" : section === "apps" ? "Workspace apps" : "Proposals and releases"}</span></div> : null}
    {section === "github" ? reminder : null}
    <div className={styles.results}>{visible.map((item, index) => <button data-pick type="button" key={`${item.surface.kind}:${item.surface.id ?? item.token}:${index}`} onClick={() => onPick(item)}><WorkspaceIcon name={item.icon} /><span>{item.label}</span><small>{item.detail}</small></button>)}
      {!visible.length && section === "github" && !data && !error ? <div className={styles.skeleton} role="status" aria-label="Loading repositories"><i /><i /><i /></div> : null}
      {!visible.length && (data || section !== "github") ? <p>No matching context.</p> : null}
      {section === "github" && data?.connection.connected ? <>{data.connection.installUrl ? <a href={data.connection.installUrl} target="_blank" rel="noopener noreferrer">Select repositories ↗</a> : null}<button type="button" onClick={() => setAttempt(value => value + 1)}>Refresh repositories</button>{data.hasMore ? <button type="button" disabled={moreLoading} onClick={() => void loadMore()}>{moreLoading ? "Loading…" : "Load more repositories"}</button> : null}</> : null}
    </div>
    {section !== "github" ? reminder : null}
    {error && section === "github" ? <div className={styles.connect} role="alert"><p>{error}</p><button type="button" onClick={() => { setError(""); if (data) void loadMore(); else setAttempt(value => value + 1); }}>Try again</button></div> : null}
  </div>;
}
