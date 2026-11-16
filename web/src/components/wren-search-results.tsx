"use client";
import Link from "next/link";
import {useEffect,useState} from "react";
import {canterFetch} from "@/lib/canter-api";
import type {Wren,WrenRoom} from "@/lib/wrens";
import {wrenSearchBytes,wrenSearchDestination,type WrenSearchResult} from "@/lib/wren-multiplayer";
import styles from "./wrens-workspace.module.css";
export function WrenSearchResults({workspaceId,query,wrens,rooms,onSelect}:{workspaceId:string;query:string;wrens:Wren[];rooms:WrenRoom[];onSelect:()=>void}){
 const [state,setState]=useState<{query:string;results:WrenSearchResult[];error:string}>({query:"",results:[],error:""});
 const q=query.trim(),bytes=wrenSearchBytes(q);
 useEffect(()=>{if(bytes<2||bytes>200)return;const controller=new AbortController();const timer=setTimeout(()=>{void canterFetch<{results:WrenSearchResult[]}>(`/workspaces/${encodeURIComponent(workspaceId)}/wren-search?q=${encodeURIComponent(q)}`,{signal:controller.signal}).then(value=>{if(!controller.signal.aborted)setState({query:q,results:value.results,error:""});}).catch(cause=>{if(!controller.signal.aborted)setState({query:q,results:[],error:cause instanceof Error?cause.message:"Search failed."});});},300);return()=>{clearTimeout(timer);controller.abort();};},[q,bytes,workspaceId]);
 if(bytes<2)return <p>Type at least two characters to search messages and files.</p>;
 if(bytes>200)return <p>Try a shorter search.</p>;
 if(state.query!==q)return <p role="status">Searching…</p>;
 return <section className={styles.searchResults} aria-label="Messages and work">{state.error?<p role="alert">{state.error}</p>:null}{state.results.length?<><p>Messages and work</p>{state.results.map(result=>{const href=wrenSearchDestination(result,wrens,rooms);return href?<Link key={`${result.kind}:${result.id}`} href={href} onClick={onSelect}><strong>{result.title}</strong><span>{result.excerpt}</span><small>{result.kind}</small></Link>:null;})}</>:!state.error?<p>No matching messages or work.</p>:null}</section>;
}
