"use client";

import {useEffect, useState} from "react";
import {usePathname, useSearchParams} from "next/navigation";
import {canterFetch} from "@/lib/canter-api";
import {githubConnectURL, type GitHubConnection, type GitHubRepositoryResult} from "@/lib/github-connection";
import {useWrenResource, workBase} from "@/lib/wren-work";
import {ProviderIcon} from "./provider-icon";
import {WorkspaceIcon} from "./workspace-icon";
import styles from "./wren-apps.module.css";

/** This uses the signed-in human's existing account connection, never a room's owner. */
export function WrenGitHubConnection({workspaceId, shared, onUse, result, bare}:{bare?:boolean;workspaceId:string;shared:boolean;onUse?:(prompt:string)=>void;result?:string}) {
  const pathname=usePathname(),query=useSearchParams();
  const base=`${workBase(workspaceId)}/github`;
  const account=useWrenResource<GitHubConnection>(base);
  const [expanded,setExpanded]=useState(false),[filter,setFilter]=useState("");
  const [repositories,setRepositories]=useState<GitHubRepositoryResult|null>(null);
  const [loading,setLoading]=useState(false),[error,setError]=useState("");
  const [confirmDisconnect,setConfirmDisconnect]=useState(false),[disconnecting,setDisconnecting]=useState(false);
  const connection=account.data;
  const nextQuery=new URLSearchParams();
  const conversation=query.get("conversation");
  if(conversation)nextQuery.set("conversation",conversation);
  nextQuery.set("app","github");
  const next=`${pathname}?${nextQuery}`;
  const connectURL=githubConnectURL(connection,workspaceId,next);
  const refreshAccount=account.refresh;
  useEffect(()=>{
    const refresh=()=>refreshAccount();
    window.addEventListener("focus",refresh);
    return()=>window.removeEventListener("focus",refresh);
  },[refreshAccount]);
  async function browse(page=1){
    if(loading)return;
    setExpanded(true);setLoading(true);setError("");
    try{
      const value=await canterFetch<GitHubRepositoryResult>(`${base}/repositories?page=${page}`);
      setRepositories(previous=>({...value,repositories:page===1?value.repositories:[...new Map([...(previous?.repositories??[]),...value.repositories].map(repo=>[repo.full_name,repo])).values()]}));
      account.refresh();
    }catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t load repositories.");}
    finally{setLoading(false);}
  }
  async function disconnect(){
    if(disconnecting)return;
    setDisconnecting(true);setError("");
    try{await canterFetch(base,{method:"DELETE"});setRepositories(null);setExpanded(false);setConfirmDisconnect(false);account.refresh();}
    catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t disconnect GitHub.");}
    finally{setDisconnecting(false);}
  }
  const visible=repositories?.repositories.filter(repo=>`${repo.full_name} ${repo.description}`.toLowerCase().includes(filter.toLowerCase()))??[];
  return <section className={bare?styles.bareAccount:styles.account} aria-label="Your GitHub account">
    {bare?null:<header className={styles.accountHeading}><span className={styles.appIcon}><ProviderIcon provider="github"/></span><div><h3>GitHub</h3><p>{connection?.connected?`Connected as ${connection.login}`:"Explore repositories and understand the code."}</p></div><span className={styles.accountLabel}>Your account</span></header>}
    {!connection&&!account.error?<p role="status">Checking your connection…</p>:null}
    {account.error?<p role="alert">{account.error} <button onClick={account.refresh}>Retry</button></p>:null}
    {result&&result!=="connected"?<p role="status">{result==="access_denied"?"Connection cancelled. Your message is still here.":"GitHub wasn’t connected. You can try again."}</p>:null}
    {connection?.connected?<>
      <p>Ask your Wren to explain a repository, read source files, or compare changes.</p>
      <div className={styles.actions}><button className={styles.primary} disabled={loading} onClick={()=>void browse()}>{loading?"Loading repositories…":"Choose a repository"}</button><button aria-expanded={expanded} onClick={()=>setExpanded(!expanded)}>{expanded?"Hide details":"Manage"}</button></div>
    </>:connection?<>
      <p>{connection.reconnect?"Reconnect to restore access to your repositories.":"Connect once to use your repositories across your chats in this workspace."}</p>
      {connection.enabled?<><a className={styles.primary} href={connectURL}><ProviderIcon provider="github"/>{connection.reconnect?"Reconnect GitHub":"Connect GitHub"}<WorkspaceIcon name="external" width="14" height="14"/></a><p className={styles.footnote}>{connection.appEnabled?"Choose which repositories Canter can read on GitHub.":"GitHub requests repository permissions including write access. Canter currently uses this connection to read source."}</p></>:<p className={styles.unavailable}>Account connection isn’t configured on this server. Public repository links still work in chat.</p>}
      {onUse?<button className={styles.example} onClick={()=>onUse("Explain how https://github.com/golang/go is organized. Read its README and cite the source files you used.")}>Try a public repository <WorkspaceIcon name="arrow" width="15" height="15"/></button>:null}
    </>:null}
    {shared?<p className={styles.footnote}>Your sign-in stays yours. Results you ask for in this room are visible to its members.</p>:null}
    {expanded&&connection?.connected?<div className={styles.repositoryBrowser}>
      {repositories?<><label>Find a repository<input type="search" value={filter} placeholder="Search loaded repositories" onChange={event=>setFilter(event.target.value)}/></label><div className={styles.repositories}>{visible.map(repo=><button key={repo.full_name} disabled={!onUse} onClick={()=>onUse?.(`Help me understand https://github.com/${repo.full_name}. Read the README and summarize its architecture, citing source files.`)}><span><strong>{repo.full_name}</strong>{repo.description?<small>{repo.description}</small>:null}</span><small>{repo.private?"Private":"Public"}</small><WorkspaceIcon name="arrow" width="15" height="15"/></button>)}</div>{!visible.length?<p>{filter?"No matching repositories in this list.":"No repositories available to this account."}</p>:null}{repositories.hasMore?<button disabled={loading} onClick={()=>void browse(repositories.page+1)}>Load more repositories</button>:null}</>:null}
      <div className={styles.actions}>{connection.installUrl?<a href={connection.installUrl} target="_blank" rel="noopener noreferrer">Manage access on GitHub ↗</a>:null}<a href={connectURL}>Reconnect</a><button onClick={()=>setConfirmDisconnect(!confirmDisconnect)}>Disconnect</button></div>
      {confirmDisconnect?<div className={styles.disconnect}><p>Disconnect your GitHub account from this workspace? Your Wrens will lose authenticated repository access. Saved messages remain.</p><button disabled={disconnecting} onClick={()=>void disconnect()}>{disconnecting?"Disconnecting…":"Disconnect GitHub"}</button><button disabled={disconnecting} onClick={()=>setConfirmDisconnect(false)}>Keep connected</button></div>:null}
    </div>:null}
    {error?<p role="alert">{error}</p>:null}
  </section>;
}
