"use client";
import { useEffect, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import type { OperatorSurface } from "@/lib/operator-api";
import { ProviderIcon } from "./provider-icon";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./repository-overview.module.css";
type Repository = {repository:string;description:string;defaultBranch:string;language:string;private:boolean;commit:string;parent?:string;files:string[];truncated:boolean};
export function RepositoryOverview({surface,workspaceId,onSelect}:{surface:OperatorSurface;workspaceId:string;onSelect:(surface:OperatorSurface)=>void}) {
 const [data,setData]=useState<Repository|null>(null);
 const [error,setError]=useState('');const [retry,setRetry]=useState(0);
 const [directory,setDirectory]=useState('');const [filter,setFilter]=useState('');
 useEffect(()=>{
  const controller=new AbortController();const query=new URLSearchParams({repository:surface.repository??'',commit:surface.id??''});
  canterFetch<Repository>(`/workspaces/${encodeURIComponent(workspaceId)}/github/inspect?${query}`,{signal:controller.signal}).then(value=>{setData(value);setError('');}).catch(cause=>{if(!controller.signal.aborted)setError(cause instanceof Error?cause.message:'Could not read repository.');});
  return()=>controller.abort();
 },[surface.repository,surface.id,workspaceId,retry]);
 const files=data?.files??[];
 const entries=filter?files.filter(path=>path.toLowerCase().includes(filter.toLowerCase())).map(path=>({path,label:path,directory:false})):[...new Map(files.filter(path=>path.startsWith(directory)).map(path=>{const tail=path.slice(directory.length);const slash=tail.indexOf('/');const isDirectory=slash>=0;const label=isDirectory?tail.slice(0,slash):tail;const value={path:directory+label+(isDirectory?'/':''),label,directory:isDirectory};return[value.path,value] as const;})).values()].sort((a,b)=>Number(b.directory)-Number(a.directory)||a.label.localeCompare(b.label));
 const manifest=files.find(path=>['package.json','go.mod','Cargo.toml','pyproject.toml','requirements.txt'].includes(path));
 const staticRoot=files.includes('index.html');
 const readme=files.find(path=>/^readme(?:\.md|\.txt|\.rst)?$/i.test(path));
 const openFile=(path:string)=>onSelect({kind:'file',repository:surface.repository,id:surface.id,path});
 return <section className={styles.overview} aria-label="Repository overview">
  <header className={styles.header}><div className={styles.identity}><ProviderIcon provider="github"/><span>{surface.repository?.split('/')[0]}</span></div><a href={`https://github.com/${surface.repository}`} target="_blank" rel="noreferrer" aria-label="Open repository on GitHub"><WorkspaceIcon name="external" width="17" height="17"/></a></header>
  <h2>{surface.repository?.split('/').slice(1).join('/')}</h2>
  {data?.description?<p className={styles.description}>{data.description}</p>:null}
  {data?<div className={styles.metadata}><span>{data.private?'Private':'Public'}</span>{data.language?<span>{data.language}</span>:null}<span>Default: {data.defaultBranch}</span><span>{data.commit.slice(0,7)}</span></div>:null}
  {error?<p className={styles.note} role="alert">{error} <button onClick={()=>setRetry(value=>value+1)}>Retry</button></p>:null}
  {!data&&!error?<p className={styles.note} role="status">Reading repository…</p>:null}
  {data?<>
   <div className={styles.actions}>{readme?<button onClick={()=>openFile(readme)}><WorkspaceIcon name="file" width="15" height="15"/>Readme</button>:null}{data.parent?<button onClick={()=>onSelect({kind:'repository-changes',repository:surface.repository,id:data.commit,base:data.parent})}><WorkspaceIcon name="activity" width="15" height="15"/>Latest changes</button>:null}</div>
   <div className={styles.runtime}><WorkspaceIcon name={staticRoot?'apps':'terminal'} width="20" height="20"/><div><strong>{staticRoot?'Static entry point found':manifest?'Build configuration found':'Source repository'}</strong><p>{staticRoot?'index.html is at the root. Canter checks the full archive before preparing a static deployment.':manifest?`${manifest} is present. A checked-in production build is required for hosted deployment.`:'Select a file to inspect it, or ask Canter to check deployment requirements.'}</p></div>{manifest?<button onClick={()=>openFile(manifest)} aria-label={`Read ${manifest}`}><WorkspaceIcon name="chevron" width="16" height="16"/></button>:null}</div>
   <section className={styles.files} aria-label="Repository files"><div className={styles.fileHeading}><h3>Files</h3><span>{files.length}{data.truncated?'+':''}</span></div><label className={styles.search}><WorkspaceIcon name="search" width="15" height="15"/><input aria-label="Find a repository file" placeholder="Find a file…" value={filter} onChange={event=>setFilter(event.target.value)}/></label>
   {directory&&!filter?<button className={styles.parent} onClick={()=>setDirectory(directory.split('/').slice(0,-2).join('/')+(directory.split('/').length>2?'/':''))}>← {directory}</button>:null}
   <div className={styles.fileList}>{entries.map(item=><button key={item.path} onClick={()=>item.directory?setDirectory(item.path):openFile(item.path)}><WorkspaceIcon name={item.directory?'folder':'file'} width="16" height="16"/><span>{item.label}</span>{item.directory?<WorkspaceIcon name="chevron" width="13" height="13"/>:null}</button>)}</div>
   {!entries.length?<p className={styles.note}>No matching files.</p>:null}{data.truncated?<p className={styles.note}>Showing the first {files.length} files. GitHub has the complete tree.</p>:null}</section>
   <p className={styles.footer}>Source pinned to {data.commit.slice(0,7)}. Files open in a highlighted code view.</p>
  </>:null}
 </section>;
}
