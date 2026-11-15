"use client";

import {WorkspaceIcon} from "./workspace-icon";
import type {AppDiscoverySnapshot} from "@/lib/wren-app-status";
import styles from "./wren-apps.module.css";

/** Render the tool's scoped evidence. Sign-in happens in the current viewer's Apps view. */
export function WrenAppsCard({onOpen,discovery}:{onOpen:(installationId?:string,catalogSlug?:string)=>void;discovery:AppDiscoverySnapshot|null}) {
  const checked=discovery?.state==="checked";
  return <section className={styles.chatCard} aria-label="Conversation apps">
    <header><WorkspaceIcon name="apps" width="17" height="17"/><strong>Apps for this conversation</strong><button onClick={()=>onOpen()}>Browse apps</button></header>
    {!checked?<p>Open Apps to check current connections and explore available sources.</p>:!discovery.connections.length?<p>No apps were added to this conversation at this check.</p>:discovery.connections.map(item=><div className={styles.chatApp} key={item.id}><span className={styles.appIcon}><WorkspaceIcon name="apps" width="20" height="20"/></span><span><strong>{item.name}</strong><small>{item.label} at this check</small></span><button aria-label={`Open ${item.name}`} onClick={()=>onOpen(item.id)}>Open</button></div>)}
    {checked&&!discovery.connections.length&&discovery.sources.length?<><p className={styles.sourceHeading}>Available public sources · No sign-in needed</p>{discovery.sources.map(item=><div className={styles.chatApp} key={item.name}><span className={styles.appIcon}><WorkspaceIcon name="apps" width="20" height="20"/></span><span><strong>{item.name}</strong><small>{item.description}</small></span><button aria-label={item.slug?`Explore ${item.name}`:`Browse apps for ${item.name}`} onClick={()=>onOpen(undefined,item.slug)}>{item.slug?"Explore":"Browse"}</button></div>)}</>:null}
    <footer>{checked?"Snapshot for the person who asked. Open Apps for current access, account connections, and setup.":"Account sign-in and adding an app are separate steps. Nothing is connected by opening this card."}</footer>
  </section>;
}
