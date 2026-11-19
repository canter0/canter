"use client";

import {useEffect,useRef,useState,useSyncExternalStore} from "react";
import {useRouter,useSearchParams} from "next/navigation";
import {canterFetch} from "@/lib/canter-api";
import {type ConversationDetail,type OperatorEvent,type OperatorSurface,conversationBase} from "@/lib/operator-api";
import {useWrenResource,workBase} from "@/lib/wren-work";
import {useWorkspace} from "./workspace-context";
import {PluginSheet} from "./global-plugin-marketplace";
import {OperatorTurn} from "./operator-turn";
import {OperatorComposer} from "./operator-composer";
import {useOperatorAttachmentDraft} from "@/lib/operator-attachment-draft";
import {WorkspaceIcon} from "./workspace-icon";
import styles from "./global-plugin-marketplace.module.css";
import chatStyles from "./operator-workspace.module.css";

type Item={id:string;kind:"rule"|"hook"|"skill";name:string;body:string;event:string;matcher:string;enabled:boolean;revision:number;action:string;description:string;source:string;sourceRevision:string};
const keepCurrent=()=>true;
const clientReady=()=>true,serverReady=()=>false;
function subscribeDraft(onChange:()=>void){
  window.addEventListener("storage",onChange);
  window.addEventListener("canter-customization-draft",onChange);
  return()=>{window.removeEventListener("storage",onChange);window.removeEventListener("canter-customization-draft",onChange);};
}
const activeConversation=(value:ConversationDetail)=>value.run?.status==="queued"||value.run?.status==="running";
const labels={rule:"Rules",hook:"Hooks",skill:"Skills"};

export function CustomizationEditor({workspaceId,kind}:{workspaceId:string;kind:Item["kind"]}) {
  const {data}=useWorkspace(),router=useRouter(),query=useSearchParams();
  const base=`${workBase(workspaceId)}/customizations`,resource=useWrenResource<{items:Item[]}>(base,3000,keepCurrent);
  const setup=query.get("setup")??"";
  const creating=query.get("create")==="1";
  const detail=useWrenResource<ConversationDetail>(setup?`${conversationBase(workspaceId)}/${encodeURIComponent(setup)}`:"",1000,activeConversation);
  const running=!!detail.data&&activeConversation(detail.data);
  const [stream,setStream]=useState<{setup:string;events:OperatorEvent[]}>({setup:"",events:[]});
  const [streamFailure,setStreamFailure]=useState({setup:"",message:""});
  const streamError=streamFailure.setup===setup?streamFailure.message:"";
  useEffect(()=>{
    if(!setup)return;
    const controller=new AbortController();let timer:ReturnType<typeof setTimeout>|undefined,cursor=0,history:OperatorEvent[]=[];
    async function poll(){
      try{
        const page=await canterFetch<{events:OperatorEvent[]}>(`${conversationBase(workspaceId)}/${encodeURIComponent(setup)}/events?after=${cursor}`,{signal:controller.signal});
        if(controller.signal.aborted)return;
        const fresh=page.events.filter(event=>event.sequence>cursor);
        if(fresh.length){cursor=Math.max(...fresh.map(event=>event.sequence));history=[...history,...fresh].slice(-2000);setStream({setup,events:history});}
        setStreamFailure({setup,message:""});timer=setTimeout(poll,fresh.length>=200?0:1000);
      }catch(cause){if(!controller.signal.aborted){setStreamFailure({setup,message:cause instanceof Error?cause.message:"Reconnecting to the agent…"});timer=setTimeout(poll,4000);}}
    }
    void poll();return()=>{controller.abort();if(timer)clearTimeout(timer);};
  },[setup,workspaceId]);
  const [selected,setSelected]=useState<Item|null>(null),[target,setTarget]=useState<Item|null>(null);
  const [showSaved,setShowSaved]=useState(false);
  const [draftFallback,setDraftFallback]=useState<{key:string;value:string}|null>(null),[busy,setBusy]=useState(false),[changing,setChanging]=useState(false),[error,setError]=useState("");
  const request=useRef<{signature:string;id:string;requestId:string}|null>(null);
  const composer=useRef<HTMLTextAreaElement>(null),lock=useRef(false);
  const transcript=useRef<HTMLDivElement>(null),followLatest=useRef(true);
  const latestEvent=stream.setup===setup?stream.events.at(-1)?.sequence:undefined;
  const latestMessage=detail.data?.messages.at(-1)?.id;
  useEffect(()=>{followLatest.current=true;},[setup]);
  useEffect(()=>{
    if(!followLatest.current)return;
    const frame=requestAnimationFrame(()=>{if(transcript.current)transcript.current.scrollTop=transcript.current.scrollHeight;});
    return()=>cancelAnimationFrame(frame);
  },[setup,latestEvent,latestMessage]);
  const draftKey=`canter:customization-draft:${data?.account.id}:${workspaceId}:${kind}:${setup||"new"}`;
  const attachments=useOperatorAttachmentDraft(data?.account.id?draftKey:null);
  const [context,setContext]=useState<OperatorSurface|null>(null);
  const draftReady=useSyncExternalStore(subscribeDraft,clientReady,serverReady);
  const draft=useSyncExternalStore(subscribeDraft,()=>{
    if(draftFallback?.key===draftKey)return draftFallback.value;
    try{return sessionStorage.getItem(draftKey)??"";}catch{return "";}
  },()=>"");
  useEffect(()=>{if(creating&&draftReady)composer.current?.focus();},[creating,draftReady]);
  function editDraft(value:string){try{sessionStorage.setItem(draftKey,value);setDraftFallback(null);window.dispatchEvent(new Event("canter-customization-draft"));}catch{setDraftFallback({key:draftKey,value});}}
  const items=resource.data?.items.filter(item=>item.kind===kind)??[];
  const conversation=detail.data?.conversation;
  const privateConversation=!conversation||!conversation.shared&&(!conversation.ownerAccountId||conversation.ownerAccountId===data?.account.id);
  function navigate(id:string){const next=new URLSearchParams({view:`${kind}s`});if(id)next.set("setup",id);else next.set("create","1");router.push(`/app/customize?${next}`,{scroll:false});}
  function openSurface(surface:OperatorSurface){
    if(surface.kind==="customization"){resource.refresh();return}
    if(surface.kind==="wren-extensions"){router.push(`/app/customize?conversationId=${encodeURIComponent(setup)}`);return}
    router.push(`/app/conversations/${encodeURIComponent(setup)}`);
  }
  async function submit(){
    const message=draft.trim()||(attachments.items.length?"Please use the attached files.":"");
    if(!message||lock.current||!privateConversation||!draftReady||!attachments.loaded)return;
    lock.current=true;setBusy(true);setError("");
    const signature=JSON.stringify({message,target:target?.id,setup,context,attachments:attachments.items});
    if(request.current?.signature!==signature)request.current={signature,id:setup||`conv_${crypto.randomUUID()}`,requestId:crypto.randomUUID()};
    const pending=request.current;
    try{
      await canterFetch(setup?`${conversationBase(workspaceId)}/${encodeURIComponent(setup)}/messages`:conversationBase(workspaceId),{method:"POST",body:JSON.stringify({id:pending.id,requestId:pending.requestId,message,attachments:attachments.items,surface:context??{kind:"customization",id:kind,...(target?{path:target.id}:{})}})});
      editDraft("");setTarget(null);setContext(null);attachments.update([]);request.current=null;
      if(!setup)navigate(pending.id);else{detail.refresh();}
    }catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t send your request. Your draft is kept.");}
    finally{lock.current=false;setBusy(false);}
  }
  async function stop(){
    if(!detail.data?.run||busy)return;setBusy(true);setError("");
    try{await canterFetch(`${conversationBase(workspaceId)}/${encodeURIComponent(setup)}/stop`,{method:"POST",body:JSON.stringify({runId:detail.data.run.id})});detail.refresh();resource.refresh();}
    catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t stop the response.");}finally{setBusy(false);}
  }
  async function change(item:Item,remove=false){
    if(changing)return;setChanging(true);setError("");
    try{const result=await canterFetch<Item>(`${base}/${item.id}`,{method:remove?"DELETE":"PATCH",body:JSON.stringify(remove?{revision:item.revision}:{...item,enabled:!item.enabled})});setSelected(remove?null:result);resource.refresh();}
    catch(cause){setError(cause instanceof Error?cause.message:"Couldn’t update this item.");resource.refresh();}
    finally{setChanging(false);}
  }
  function askToChange(item:Item){setTarget(item);setContext(null);setSelected(null);requestAnimationFrame(()=>composer.current?.focus());}
  const savedItems=<section className={styles.itemList} aria-label={`Saved ${labels[kind].toLowerCase()}`}>{items.map(item=><button className={styles.row} key={item.id} onClick={()=>{setSelected(item);setShowSaved(false);setError("");}}><WorkspaceIcon name={kind==="hook"?"terminal":kind==="skill"?"activity":"file"} width="18" height="18"/><span className={styles.rowMeta}><strong>{item.name}</strong><small>{!item.enabled?"Disabled":item.description||item.kind==="rule"&&"Always active"||item.event==="PostToolUse"&&"After tool use"||item.event==="ToolFailure"&&"When a tool fails"||item.event==="PreToolUse"&&"Before tool use"||item.event==="UserPromptSubmit"&&"On each request"||"Available to your agent"}</small></span></button>)}</section>;
  return <div className={styles.customizationChat}>
    <header className={styles.head}><h1>{labels[kind]}</h1><div className={styles.headActions}>{items.length>0?<button aria-label={`Saved ${labels[kind].toLowerCase()}`} title={`Saved ${labels[kind].toLowerCase()}`} onClick={()=>setShowSaved(true)}><WorkspaceIcon name="file" width="18" height="18"/></button>:null}<button aria-label={`Create ${kind}`} title={`Create ${kind}`} disabled={busy} onClick={()=>{navigate("");setTarget(null);setContext(null);setError("");requestAnimationFrame(()=>composer.current?.focus());}}><WorkspaceIcon name="plus" width="18" height="18"/></button></div></header>
    <div ref={transcript} className={`${chatStyles.transcript} ${styles.customizationTranscript}`} onScroll={event=>{const node=event.currentTarget;followLatest.current=node.scrollHeight-node.scrollTop-node.clientHeight<64;}}>
    {!setup?<div className={`${chatStyles.conversationWelcome} ${styles.customizationWelcome}`} data-visible="true"><h2 className={chatStyles.startBrand}>Create your own {kind}</h2></div>:null}
    {setup?<div className={styles.setupChat} aria-label="Setup conversation">
      {detail.loading&&!detail.data?<p role="status" className={styles.muted}>Opening your conversation…</p>:null}
      {detail.error?<p role="alert" className={styles.error}>{detail.error} <button onClick={detail.refresh}>Retry</button></p>:null}
      {!privateConversation?<p role="alert" className={styles.error}>Open a private conversation to configure your customizations.</p>:detail.data?.messages.filter(message=>message.role==="user").map(message=><OperatorTurn key={message.id} message={{...message,surface:null}} answer={detail.data?.messages.find(answer=>answer.role==="assistant"&&answer.runId===message.runId)} events={(stream.setup===setup?stream.events:[]).filter(event=>event.runId===message.runId)} running={running&&message.runId===detail.data?.run?.id} onSelect={openSurface} conversations={data?.conversations??[]}/>)}
      {streamError?<p role="status" className={styles.muted}>{streamError} Reconnecting…</p>:null}
      {detail.data?.run?.status==="failed"?<p className={styles.error} role="alert">{detail.data.run.failure||"The agent could not finish. You can continue below."}</p>:null}
      {detail.data?.run?.status==="cancelled"?<p className={styles.muted}>Stopped. Saved changes remain available.</p>:null}
    </div>:null}
    </div>
    <div className={chatStyles.composerArea}>
      {attachments.error?<p role="status" className={styles.muted}>{attachments.error}</p>:null}
      {error&&!selected?<p role="alert" className={styles.error}>{error}</p>:null}
      {resource.error?<p role="alert" className={styles.error}>Couldn’t load saved {labels[kind].toLowerCase()}. <button onClick={resource.refresh}>Retry</button></p>:null}
      {target?<div className={styles.setupTarget}><span>Changing {target.name}</span><button type="button" aria-label="Clear selected customization" onClick={()=>setTarget(null)}><WorkspaceIcon name="close" width="14" height="14"/></button></div>:null}
      {draftReady?<OperatorComposer placeholder={`Create your ${kind}`} dictationScope={draftKey} draft={draft} onChange={editDraft} onSend={()=>void submit()} onStop={()=>void stop()} running={running} stopping={busy&&running} disabled={busy||!attachments.loaded||!privateConversation||data?.workspace.role==="viewer"} sending={busy&&!running} inputRef={composer} managedModel modelOptions={{}} onModelChange={()=>{}} onModelOptionsChange={()=>{}} onSelect={setContext} context={context} onClearContext={()=>setContext(null)} attachments={attachments.items} onAttachments={attachments.update} conversationId={setup||undefined}/>:null}
    </div>
    {showSaved?<PluginSheet title={`Saved ${labels[kind].toLowerCase()}`} onClose={()=>setShowSaved(false)}>{savedItems}</PluginSheet>:null}
    {selected?<PluginSheet title={selected.name} onClose={()=>{if(!changing)setSelected(null);}}>
      <div className={styles.form}><p className={styles.muted}>{selected.enabled?"Enabled":"Disabled"}</p>
        {selected.kind==="hook"?<p className={styles.muted}>{selected.event==="PostToolUse"?"After a tool succeeds":selected.event==="ToolFailure"?"When a tool fails":selected.event==="PreToolUse"?"Before a tool runs":"When you send a request"}{selected.matcher?` · ${selected.matcher}`:""}</p>:null}
        <div className={styles.savedInstructions}>{selected.body}</div>
        {selected.source?<p className={styles.muted}>From <a href={`https://github.com/${selected.source}/tree/${selected.sourceRevision}`} target="_blank" rel="noreferrer">{selected.source} ↗</a></p>:null}
        {error?<p role="alert" className={styles.error}>{error}</p>:null}
        <button className={styles.connect} disabled={changing} onClick={()=>askToChange(selected)}>Ask Canter to change this</button>
        <button className={styles.textButton} disabled={changing} onClick={()=>change(selected)}>{selected.enabled?"Disable":"Enable"}</button>
        <button className={styles.textButton} disabled={changing} onClick={()=>change(selected,true)}>Delete {kind}</button>
      </div>
    </PluginSheet>:null}
  </div>;
}
