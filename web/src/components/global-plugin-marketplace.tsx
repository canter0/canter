"use client";

import Link from "next/link";
import Image from "next/image";
import {useCallback,useEffect,useRef,useState,type FormEvent,type ReactNode} from "react";
import {useRouter,useSearchParams} from "next/navigation";
import {canterFetch} from "@/lib/canter-api";
import {useWrenResource,workBase} from "@/lib/wren-work";
import {pluginStatus,type MarketplacePlugin,type PluginCatalog,type PluginConnection,type PluginTool} from "@/lib/plugin-marketplace";
import {useWorkspace} from "./workspace-context";
import {WorkspaceIcon} from "./workspace-icon";
import styles from "./global-plugin-marketplace.module.css";

function PluginIcon({plugin}:{plugin:MarketplacePlugin}) {
  const [failed,setFailed]=useState(false);
  return plugin.icon&&!failed?<Image unoptimized className={styles.tile} src={plugin.icon} onError={()=>setFailed(true)} alt="" width={40} height={40}/>:<span className={styles.monogram} aria-hidden="true">{plugin.name.slice(0,2)}</span>;
}
export function PluginSheet({title,onClose,children}:{title:string;onClose:()=>void;children:ReactNode}) {
  const ref=useRef<HTMLDialogElement>(null),opener=useRef<HTMLElement|null>(null);
  useEffect(()=>{opener.current=document.activeElement as HTMLElement;const node=ref.current;node?.showModal();return()=>{node?.close();opener.current?.focus({preventScroll:true});};},[]);
  return <dialog ref={ref} className={styles.drawer} aria-label={title} onCancel={event=>{event.preventDefault();onClose();}} onClick={event=>{const bounds=ref.current?.getBoundingClientRect();if(event.target===ref.current&&bounds&&event.clientX<bounds.left)onClose();}}>
    <div className={styles.drawerBar}><span>{title}</span><button type="button" aria-label="Close details" onClick={onClose}><WorkspaceIcon name="close" width="18" height="18"/></button></div>{children}
  </dialog>;
}
export function GlobalPluginMarketplace({custom=false}:{custom?:boolean}) {
  const {data}=useWorkspace(),workspace=data?.workspace.id??"",base=workspace?workBase(workspace):"";
  const catalog=useWrenResource<PluginCatalog>(base?`${base}/plugin-marketplace`:"");
  const installed=useWrenResource<{connections:PluginConnection[]}>(base?`${base}/plugins`:"");
  const router=useRouter(),params=useSearchParams(),slug=params.get("plugin")??"";
  const [query,setQuery]=useState(params.get("q")??""),[searchOpen,setSearchOpen]=useState(!!query);
  const search=useRef<HTMLInputElement>(null);
  useEffect(()=>{if(searchOpen)search.current?.focus();},[searchOpen]);
  const connection=installed.data?.connections.find(item=>item.catalog.slug===slug);
  const plugin=connection?.catalog??catalog.data?.plugins.find(item=>item.slug===slug);
  const connections=(installed.data?.connections??[]).filter(item=>item.enabled||item.state!=="installed");
  const plugins=(catalog.data?.plugins??[]).filter(item=>`${item.name} ${item.summary} ${item.description}`.toLowerCase().includes(query.toLowerCase())).sort((a,b)=>Number(b.slug==="deepwiki")-Number(a.slug==="deepwiki"));
  const navigate=(selected:string)=>{const next=new URLSearchParams();if(query)next.set("q",query);if(selected)next.set("plugin",selected);router.push(`/app/customize${next.size?`?${next}`:""}`,{scroll:false});};
  return <div className={styles.layout}><div className={styles.content}>
    <header className={styles.head}><h1>Plugins</h1><div className={styles.headActions}>
      <button aria-label="Search plugins" aria-pressed={searchOpen} onClick={()=>setSearchOpen(value=>!value)}><WorkspaceIcon name="search" width="17" height="17"/></button>
      <Link href="/app/account/connections" aria-label="Account connections"><WorkspaceIcon name="gear" width="17" height="17"/></Link>
      <button aria-label="Add custom connection" onClick={()=>router.push("/app/customize?view=custom",{scroll:false})}><WorkspaceIcon name="plus" width="17" height="17"/></button>
    </div></header>
    {searchOpen?<input ref={search} className={styles.search} type="search" aria-label="Search plugins" placeholder="Find a plugin or capability…" value={query} onChange={e=>setQuery(e.target.value)}/>:null}
    {installed.error?<p role="alert" className={styles.error}>Couldn’t load your plugins. <button onClick={installed.refresh}>Retry</button></p>:null}
    {connections.length>0?<section className={styles.installed} aria-label="Installed"><h2>Installed <span>{connections.length}</span></h2><div className={styles.grid}>{connections.filter(item=>item.catalog.name.toLowerCase().includes(query.toLowerCase())).map(item=><button key={item.id} className={styles.row} onClick={()=>navigate(item.catalog.slug)}><PluginIcon plugin={item.catalog}/><span className={styles.rowMeta}><strong>{item.catalog.name}</strong><small>{pluginStatus(item)}</small></span></button>)}</div></section>:null}
    <section aria-label="Marketplace"><h2 className={styles.marketHead}>Marketplace</h2>
      {catalog.loading&&!catalog.data?<p role="status" className={styles.muted}>Loading marketplace…</p>:null}
      {catalog.error?<p role="alert" className={styles.error}>Couldn’t load the marketplace. <button onClick={catalog.refresh}>Retry</button></p>:null}
      {catalog.data?.warning?<p role="status" className={styles.muted}>{catalog.data.warning}</p>:null}
      {!plugins.length&&catalog.data?<p role="status" className={styles.muted}>No plugins match “{query}”.</p>:null}
      <div className={styles.grid}>{plugins.map(item=><button className={styles.row} key={item.slug} onClick={()=>navigate(item.slug)}><PluginIcon plugin={item}/><span className={styles.rowMeta}><strong>{item.name}</strong><small>{item.summary||item.description}</small></span></button>)}</div>
    </section>
    {plugin?<PluginSheet title={plugin.name} onClose={()=>navigate("")}><PluginDetail key={`${workspace}:${plugin.slug}`} plugin={plugin} initial={connection} base={base} onChanged={installed.refresh} callbackStatus={params.get("connection")??""}/></PluginSheet>:null}
    {custom?<PluginSheet title="Custom connection" onClose={()=>router.push("/app/customize",{scroll:false})}><CustomPlugin base={base} onCreated={value=>{installed.refresh();navigate(value.catalog.slug);}}/></PluginSheet>:null}
  </div></div>;
}
function CustomPlugin({base,onCreated}:{base:string;onCreated:(value:PluginConnection)=>void}){
  const [name,setName]=useState(""),[endpoint,setEndpoint]=useState(""),[busy,setBusy]=useState(false),[error,setError]=useState("");
  async function submit(event:FormEvent){event.preventDefault();if(busy)return;setBusy(true);setError("");try{onCreated(await canterFetch<PluginConnection>(`${base}/plugins`,{method:"POST",body:JSON.stringify({name,endpoint})}));}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t add this connection.");}finally{setBusy(false);}}
  return <form className={styles.form} onSubmit={submit}><p className={styles.muted}>Add a hosted MCP server. Canter checks its connection and prompts for sign-in or credentials.</p><label>Name<input required maxLength={100} value={name} onChange={e=>setName(e.target.value)} disabled={busy}/></label><label>Server URL<input type="url" required placeholder="https://example.com/mcp" value={endpoint} onChange={e=>setEndpoint(e.target.value)} disabled={busy}/></label>{error?<p className={styles.error} role="alert">{error}</p>:null}<button className={styles.connect} disabled={busy}>{busy?"Adding…":"Add connection"}</button></form>;
}
function PluginDetail({plugin,initial,base,onChanged,callbackStatus}:{plugin:MarketplacePlugin;initial?:PluginConnection;base:string;onChanged:()=>void;callbackStatus:string}) {
  const [connection,setConnection]=useState(initial),[busy,setBusy]=useState(false),[error,setError]=useState("");
  const [values,setValues]=useState<Record<string,string>>({}),[token,setToken]=useState(""),[tokenForm,setTokenForm]=useState(false);
  const [server,setServer]=useState(plugin.servers.find(item=>item.url)?.name??"");
  const [tools,setTools]=useState<PluginTool[]>([]),[chosen,setChosen]=useState<string[]>([]);
  const [clientID,setClientID]=useState(""),[registration,setRegistration]=useState("");
  const [editing,setEditing]=useState(false);
  const inputs=plugin.inputs,hosted=plugin.servers.filter(item=>item.url),supported=hosted.length>0;
  const refreshTools=useCallback((id:string,signal?:AbortSignal)=>canterFetch<{connection:PluginConnection;tools:PluginTool[]}>(`${base}/plugins/${encodeURIComponent(id)}`,{signal}).then(result=>{
    if(signal?.aborted)return;
    setConnection(result.connection);setTools(result.tools??[]);setChosen((result.tools??[]).slice(0,8).map(item=>item.name));
  }),[base]);
  useEffect(()=>{if(!initial?.id)return;const controller=new AbortController();refreshTools(initial.id,controller.signal).catch(cause=>{if(!controller.signal.aborted)setError(cause.message);});return()=>controller.abort();},[initial?.id,refreshTools]);
  const save=async(value:PluginConnection)=>{setConnection(value);onChanged();if(value.state==="review")await refreshTools(value.id);};
  async function action(task:()=>Promise<void>){if(busy)return;setBusy(true);setError("");try{await task();}catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t complete setup.");}finally{setBusy(false);setToken("");}}
  async function configure(current=connection){
    if(!current)return;
    const result=await canterFetch<PluginConnection>(`${base}/plugins/${current.id}/configure`,{method:"POST",body:JSON.stringify({revision:current.revision,server,values,token})});
    setValues({});setEditing(false);await save(result);
  }
  async function install(){
    await action(async()=>{
      const value=await canterFetch<PluginConnection>(`${base}/plugins`,{method:"POST",body:JSON.stringify({slug:plugin.slug})});await save(value);
      if(value.state==="installed"&&value.enabled&&!inputs.some(item=>item.required&&!item.default)&&hosted.length===1)await configure(value);
    });
  }
  function submit(event:FormEvent){event.preventDefault();void action(()=>configure());}
  async function authorize(){
    if(!connection)return;
    await action(async()=>{
      const result=await canterFetch<{authorizationUrl?:string;clientRegistrationRequired?:boolean;redirectUrl?:string}>(`${base}/plugins/${connection.id}/authorize`,{method:"POST",body:JSON.stringify({revision:connection.revision,clientId:clientID})});
      if(result.clientRegistrationRequired){setRegistration(result.redirectUrl??"");return}
      if(result.authorizationUrl){const url=new URL(result.authorizationUrl);if(url.protocol!=="https:"&&url.hostname!=="localhost"&&url.hostname!=="127.0.0.1")throw new Error("Invalid sign-in URL.");window.location.assign(url.href);}
    });
  }
  const post=(suffix:string,body:Record<string,unknown>={})=>action(async()=>{if(connection)await save(await canterFetch<PluginConnection>(`${base}/plugins/${connection.id}/${suffix}`,{method:"POST",body:JSON.stringify({revision:connection.revision,...body})}));});
  const setup=connection?.enabled&&(connection.state==="installed"||editing);
  return <>
    <div className={styles.detailHead}><PluginIcon plugin={plugin}/><div><h2>{plugin.name}</h2><p className={styles.muted}>{plugin.description}</p></div></div>
    {error?<p role="alert" className={styles.error}>{error}</p>:null}
    {callbackStatus&&callbackStatus!=="connected"?<p role="alert" className={styles.error}>{callbackStatus==="cancelled"?"Sign-in was cancelled. You can try again.":"Sign-in did not finish. Try connecting again."}</p>:null}
    {plugin.notice?<p className={styles.muted}>{plugin.notice}</p>:null}
    {!supported?<p className={styles.muted}>This plugin requires a local runtime. Its hosted connection is not available in Canter.</p>:!connection?<button className={styles.connect} disabled={busy} onClick={install}>{busy?"Installing…":"Install for me"}</button>:null}
    {connection&&!connection.enabled?<button className={styles.connect} disabled={busy} onClick={()=>action(async()=>save(await canterFetch<PluginConnection>(`${base}/plugins/${connection.id}`,{method:"PATCH",body:JSON.stringify({revision:connection.revision,enabled:true})})))}>Enable plugin</button>:null}
    {setup?<form className={styles.form} onSubmit={submit}>
      {hosted.length>1?<label>Connection<select value={server} disabled={busy} onChange={event=>setServer(event.target.value)}>{hosted.map(item=><option key={item.name}>{item.name}</option>)}</select></label>:null}
      {inputs.map(field=><label key={field.name}>{field.label||field.name}{!field.required?<span className={styles.muted}> · Optional</span>:null}<input type={field.sensitive?"password":"text"} autoComplete="off" required={field.required} disabled={busy} maxLength={4096} value={values[field.name]??field.default??""} onChange={event=>setValues(current=>({...current,[field.name]:event.target.value}))}/>{field.help?<small>{field.help}</small>:null}</label>)}
      {plugin.steps?.length?<ol>{plugin.steps.map((step,index)=><li key={index}>{step}</li>)}</ol>:null}
      <button className={styles.connect} disabled={busy}>{busy?"Checking connection…":"Continue"}</button>
    </form>:null}
    {connection?.enabled&&connection.state==="needs_auth"&&!editing?<div className={styles.form}>
      <p className={styles.muted}>Connect your {plugin.name} account to continue.</p>
      {registration?<><label>OAuth client ID<input required value={clientID} onChange={e=>setClientID(e.target.value)} disabled={busy}/></label><p className={styles.muted}>Register this redirect URL with the provider:</p><code className={styles.code}>{registration}</code></>:null}
      <button className={styles.connect} disabled={busy||!!registration&&!clientID} onClick={authorize}>{busy?"Connecting…":`Sign in to ${plugin.name}`}</button>
      <button className={styles.textButton} disabled={busy} onClick={()=>setTokenForm(value=>!value)}>Use an API token</button>
      {tokenForm?<form onSubmit={event=>{event.preventDefault();void post("credential",{token});}}><label>API token<input type="password" required autoComplete="new-password" minLength={8} maxLength={4096} value={token} onChange={e=>setToken(e.target.value)} disabled={busy}/></label><button className={styles.connect} disabled={busy}>Connect</button></form>:null}
      {inputs.length>0?<button className={styles.textButton} disabled={busy} onClick={()=>setEditing(true)}>Edit setup fields</button>:null}
      <small>Your connection stays personal. Room members do not inherit access.</small>
    </div>:null}
    {connection?.enabled&&connection.state==="review"&&!editing?<section className={styles.form} aria-label="Review plugin access">
      <h3>Choose tools</h3><p className={styles.muted}>Enable up to eight tools for your private chats. Tools that can make changes ask for approval before each action.</p>
      {tools.map(tool=><label key={tool.name} className={styles.tool}><input type="checkbox" checked={chosen.includes(tool.name)} disabled={busy||chosen.length>=8&&!chosen.includes(tool.name)} onChange={event=>setChosen(current=>event.target.checked?[...current,tool.name]:current.filter(name=>name!==tool.name))}/><span><strong>{tool.remoteName||tool.name}</strong><small>{tool.description}</small><small>{tool.impact==="read"?"Read-only":"Approval before use"}</small></span></label>)}
      <button className={styles.connect} disabled={busy||!chosen.length} onClick={()=>post("activate",{tools:chosen})}>{busy?"Enabling…":"Enable selected tools"}</button>
    </section>:null}
    {connection?.enabled&&connection.state==="ready"&&!editing?<section className={styles.form}><p role="status" className={styles.connected}><WorkspaceIcon name="check" width="16" height="16"/>Connected</p><p className={styles.muted}>Available to Canter in your private conversations. Ask your agent to use {plugin.name}.</p><button className={styles.textButton} disabled={busy} onClick={()=>post("retry")}>Check connection and manage tools</button></section>:null}
    {connection?.enabled&&(connection.state==="failed"||connection.state==="checking")&&!editing?<div className={styles.form}><p role="alert">{connection.failure||"The previous connection check did not finish."}</p><button className={styles.connect} disabled={busy} onClick={()=>post("retry")}>Retry connection</button><button className={styles.textButton} disabled={busy} onClick={()=>setEditing(true)}>Edit setup</button></div>:null}
    {connection&&connection.enabled?<button className={styles.textButton} disabled={busy} onClick={()=>action(async()=>save(await canterFetch<PluginConnection>(`${base}/plugins/${connection.id}`,{method:"DELETE",body:JSON.stringify({revision:connection.revision})})))}>Disconnect and remove access</button>:null}
    <details className={styles.advanced}><summary>Advanced</summary><dl><dt>Source</dt><dd><a href={plugin.source} target="_blank" rel="noreferrer">View manifest ↗</a></dd>{plugin.homepage?<><dt>Website</dt><dd><a href={plugin.homepage} target="_blank" rel="noreferrer">Open ↗</a></dd></>:null}<dt>Version</dt><dd>{plugin.revision}</dd><dt>Connections</dt><dd>{plugin.servers.map(server=>server.name).join(", ")||"None"}</dd></dl></details>
  </>;
}
