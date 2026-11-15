"use client";

import {useRef,useState,type FormEvent} from "react";
import {canterFetch} from "@/lib/canter-api";
import {notifyWorkUpdate,workBase} from "@/lib/wren-work";
import type {ExtensionTool,WrenExtension,WrenAppAccount,ExtensionInstallation} from "@/lib/wren-extensions";
import type {KnowledgeTarget} from "@/lib/wren-knowledge";
import styles from "./wren-extensions-panel.module.css";

export type MCPDiscovery={name:string;origin:string;endpoint:string;auth:"none"|"bearer";tools:ExtensionTool[];warnings:string[]};
export function WrenMCPConnection({workspaceId,onSaved,initialDiscovery,appAccount,target,onInstalled}:{workspaceId:string;onSaved:()=>void;initialDiscovery?:MCPDiscovery;appAccount?:WrenAppAccount;target?:KnowledgeTarget|null;onInstalled?:(value:ExtensionInstallation)=>void}){
 const [endpoint,setEndpoint]=useState(""),[token,setToken]=useState(""),[discovery,setDiscovery]=useState<MCPDiscovery|null>(initialDiscovery??null);
 const [name,setName]=useState(initialDiscovery?.name??""),[selected,setSelected]=useState<Record<string,ExtensionTool["impact"]>>({});
 const [requiresToken,setRequiresToken]=useState(false);
 const [busy,setBusy]=useState(false),[error,setError]=useState(""),[saved,setSaved]=useState(false);
 const heading=useRef<HTMLHeadingElement>(null),attempt=useRef<string|null>(null),installationAttempt=useRef<string|null>(null);
 const base=`${workBase(workspaceId)}/wren-extensions`,count=Object.keys(selected).length;
 async function discover(event:FormEvent){
  event.preventDefault();if(busy)return;setBusy(true);setError("");
  try{
   const result=await canterFetch<MCPDiscovery>(`${base}/mcp/discover`,{method:"POST",body:JSON.stringify({endpoint,token:requiresToken?token:""})});
   setDiscovery(result);setName(result.name);setSelected({});setSaved(false);attempt.current=null;
   requestAnimationFrame(()=>heading.current?.focus());
  }catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t read this server’s tools.");}
  finally{setToken("");setBusy(false);}
 }
 async function publish(event:FormEvent){
  event.preventDefault();if(!discovery||busy||!count||count>8)return;setBusy(true);setError("");
  try{
   attempt.current??=`extension_${crypto.randomUUID()}`;
   const extension=await canterFetch<WrenExtension>(base,{method:"POST",body:JSON.stringify({id:attempt.current,expectedVersion:0,manifest:{name,description:`Tools from ${new URL(discovery.endpoint).hostname}.`,origin:discovery.origin,auth:discovery.auth,events:[],tools:discovery.tools.filter(tool=>selected[tool.name]).map(tool=>({...tool,impact:selected[tool.name]}))}})});
   if(appAccount&&target&&onInstalled){
    installationAttempt.current??=`extinstall_${crypto.randomUUID()}`;
    const installed=await canterFetch<ExtensionInstallation>(`${base}/installations`,{method:"POST",body:JSON.stringify({id:installationAttempt.current,...target.scope,extensionId:extension.id,version:extension.version,expectedRevision:0,enabled:true,allowWrite:false,credentialMode:"personal"})});
    const linked=await canterFetch<ExtensionInstallation>(`${base}/installations/${encodeURIComponent(installed.id)}/credentials`,{method:"POST",body:JSON.stringify({expectedRevision:installed.credentialRevision,appAccountId:appAccount.id})});
    onInstalled(linked);
   }
   setSaved(true);onSaved();notifyWorkUpdate(workspaceId);
  }catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t publish this connection.");}finally{setBusy(false);}
 }
 return <section className={styles.mcpSetup} aria-label="Connect an MCP server">
 {!discovery?<form onSubmit={discover}>
  <label>Server URL<input type="url" required placeholder="https://your-service.com/mcp" value={endpoint} onChange={event=>setEndpoint(event.target.value)} disabled={busy}/></label>
  <label className={styles.checkbox}><input type="checkbox" checked={requiresToken} disabled={busy} onChange={event=>{setRequiresToken(event.target.checked);setToken("");}}/>Server requires a bearer token</label>
  {requiresToken?<><label>Bearer token<input type="password" autoComplete="new-password" required minLength={8} maxLength={4096} value={token} onChange={event=>setToken(event.target.value)} disabled={busy}/></label><p>Used only to discover tools, then cleared. Each person adds their own token after enabling the connection. OAuth sign-in is not supported for custom servers yet.</p></>:null}
  <button className={styles.primary} disabled={busy||!endpoint.trim()}>{busy?"Reading available tools…":"Find tools"}</button>
 </form>:<form onSubmit={publish}>
  <h4 ref={heading} tabIndex={-1}>Review tools</h4><p>{appAccount?`Using ${appAccount.label}`:discovery.endpoint}<br/>{appAccount?"Your teammates connect their own accounts.":discovery.auth==="none"?"No account or token required":"Each person will need their own bearer token"}</p>
  {saved?<p role="status">{appAccount?`${name} is ready in this conversation.`:`${name} is now available under Discover. Add it to a chat or room to use it${discovery.auth==="bearer"?", then add your personal token in Manage connection":""}.`}</p>:<>
   <label>Connection name<input required maxLength={100} value={name} onChange={event=>setName(event.target.value)} disabled={busy}/></label>
   <p>Choose up to eight tools. Review what each can do; server descriptions are supplied by the provider. {appAccount?`These tools use ${appAccount.label} for your requests. Other people connect their own accounts. Adding them makes the tools available in this ${target?.scope.scope==="room"?"room":"chat"}.`:"Tools start with change access until you explicitly classify them as read-only. Publishing alone gives no conversation access."}</p>
   <div className={styles.mcpTools}>{discovery.tools.map(tool=><article key={tool.name}>
    <label className={styles.checkbox}><input type="checkbox" checked={!!selected[tool.name]} disabled={busy||(!selected[tool.name]&&count>=8)} onChange={event=>setSelected(current=>{const next={...current};if(event.target.checked)next[tool.name]=appAccount?tool.impact:"write";else delete next[tool.name];return next;})}/><strong>{tool.remoteName??tool.name}</strong></label>
    <p>{tool.description}</p>
    {selected[tool.name]&&!appAccount?<label>Access for {tool.remoteName??tool.name}<select value={selected[tool.name]} disabled={busy} onChange={event=>setSelected(current=>({...current,[tool.name]:event.target.value as ExtensionTool["impact"]}))}><option value="write">Can make changes</option><option value="read">Read-only — verified by me</option></select></label>:null}
    <details><summary>Tool parameters</summary><pre>{JSON.stringify(tool.inputSchema,null,2)}</pre></details>
   </article>)}</div>
   {discovery.warnings.map((warning,index)=><p key={index}>{warning}</p>)}
   {!discovery.tools.length?<p>No supported tools were found. Check that this is the service’s MCP endpoint.</p>:null}
   <button className={styles.primary} disabled={busy||!name.trim()||!count||count>8||!!appAccount&&!target}>{busy?"Adding…":appAccount?`Add ${count||"selected"} ${count===1?"tool":"tools"} to this ${target?.scope.scope==="room"?"room":"chat"}`:`Publish ${count||"selected"} ${count===1?"tool":"tools"}`}</button>
  </>}
  {!appAccount?<button type="button" disabled={busy} onClick={()=>{setDiscovery(null);setError("");setSaved(false);}}>Choose another server</button>:null}
 </form>}
 {error?<p role="alert" className={styles.error}>{error}</p>:null}
 </section>;
}
