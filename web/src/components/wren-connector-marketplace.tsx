"use client";

import {useEffect, useRef, useState} from "react";
import type {KnowledgeTarget} from "@/lib/wren-knowledge";
import {installedSource} from "@/lib/wren-apps";
import {addCatalogConnector} from "@/lib/wren-apps-api";
import {extensionInstallationLabel, extensionToolSummary, type ConnectorCatalogEntry, type ExtensionInstallation} from "@/lib/wren-extensions";
import {workBase} from "@/lib/wren-work";
import styles from "./wren-extensions-panel.module.css";

export type ConnectorBrowseState={query:string;category:string;selected:string};

export function WrenConnectorMarketplace({workspaceId,target,writer,installations,onInstalled,onManage,browse,onBrowseChange,entries,onUse,bare}:{bare?:boolean;workspaceId:string;target:KnowledgeTarget|null;writer:boolean;installations:ExtensionInstallation[];onInstalled:(value:ExtensionInstallation)=>void;onManage:(id:string)=>void;browse:ConnectorBrowseState;onBrowseChange:(value:ConnectorBrowseState)=>void;entries:ConnectorCatalogEntry[];onUse?:(prompt:string)=>void}) {
  const base=`${workBase(workspaceId)}/wren-extensions`;
  const {selected}=browse;
  const [busy,setBusy]=useState(false),[error,setError]=useState("");
  // Reuse the installation identity after an interrupted response; never silently
  // create another grant because the first response was lost.
  const attempt=useRef<{slug:string;id:string}|null>(null);
  const entry=entries.find(value=>value.slug===selected);
  const detailHeading=useRef<HTMLElement|null>(null);
  useEffect(()=>{if(entry)detailHeading.current?.focus();},[entry]);
  const installed=(value:ConnectorCatalogEntry)=>installedSource(installations,value);
  async function add(value:ConnectorCatalogEntry){
    if(busy||!writer||!target)return;
    setBusy(true);setError("");
    try{
      if(attempt.current?.slug!==value.slug)attempt.current={slug:value.slug,id:`extinstall_${crypto.randomUUID()}`};
      const result=await addCatalogConnector(base,target,value,attempt.current.id);
      onInstalled(result);
    }catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t add this connector. Try again.");}finally{setBusy(false);}
  }
  return <section className={styles.marketplace} aria-label={entry?`${entry.manifest.name} setup`:"Public sources"} data-detail={!!entry} hidden={!entries.length}>
    {!entry?<><div className={styles.sectionHeading}><h3>Public sources</h3><small>No sign-in needed</small></div>
    <p>Search public information with source links in your conversation.</p>
    <div className={styles.catalogGrid}>{entries.map(value=>{
      const existing=installed(value);
      return <button type="button" className={styles.connectorCard} key={value.slug} onClick={()=>{onBrowseChange({...browse,selected:selected===value.slug?"":value.slug});setError("");}}>
        <span className={styles.cardTop}><span className={styles.connectorMonogram} aria-hidden="true">{value.slug==="github-public"?"GH":value.slug==="deepwiki"?"DW":"Cr"}</span><small>{existing?extensionInstallationLabel(existing):value.category}</small></span>
        <strong>{value.manifest.name}</strong><span>{value.manifest.description}</span><small>No account needed · Read access</small>
      </button>;
    })}</div></>:null}
    {entry?<div className={styles.connectorReview} id={`connector-${entry.slug}`} ref={bare?node=>{detailHeading.current=node;}:undefined} tabIndex={bare?-1:undefined}>
      {bare?null:<><button type="button" onClick={()=>onBrowseChange({...browse,selected:""})}>Back to apps</button><h2 ref={node=>{detailHeading.current=node;}} tabIndex={-1} className={styles.detailTitle}>{entry.manifest.name}</h2></>}
      <p>{entry.manifest.description}</p><p>{entry.limitation}</p>
      {entry.manifest.tools.length>1?<ul>{entry.manifest.tools.map(tool=><li key={tool.name}>{extensionToolSummary(tool)}</li>)}</ul>:null}
      <p>Queries go to {new URL(entry.manifest.origin).hostname}. This connection cannot modify remote data.</p>
      {target?<p><strong>{target.label}</strong><br/>{target.scope.scope==="room"?"Current room members and its Wrens can use this connection. Private conversations keep their own connections.":"Only this private conversation can use this connection."}</p>:<p>Open a conversation to add a connection.</p>}
      <div className={styles.actions}>{installed(entry)?.enabled&&!installed(entry)?.extension.disabled&&onUse?<button type="button" className={styles.primary} onClick={()=>onUse(entry.slug==="deepwiki"?"Use DeepWiki to explain the modelcontextprotocol/go-sdk repository’s architecture. Include source links and distinguish generated documentation from code you have verified.":entry.slug==="crossref"?"Find three research publications about attention mechanisms using Crossref. Include authors, dates, and DOI links; distinguish abstracts or metadata from papers you have read.":"Find three public GitHub issues about Go performance. Summarize each with its status and source link.")}>Try in chat</button>:null}{installed(entry)?<button type="button" onClick={()=>onManage(installed(entry)!.id)}>Manage connection</button>:<button type="button" className={styles.primary} disabled={busy||!writer||!target} onClick={()=>void add(entry)}>{busy?"Adding…":target?.scope.scope==="room"?"Add to this room":"Add to this chat"}</button>}<a href={entry.documentation} target="_blank" rel="noreferrer">Provider documentation ↗</a></div>
      {!writer?<p>A workspace member with editing access can add connections.</p>:null}
      {error?<p role="alert">{error}</p>:null}
    </div>:null}
  </section>;
}
