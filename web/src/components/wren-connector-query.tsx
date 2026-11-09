"use client";

import type {ExtensionTool} from "@/lib/wren-extensions";
import styles from "./wren-extensions-panel.module.css";

function object(value:unknown):value is Record<string,unknown>{return !!value&&typeof value==="object"&&!Array.isArray(value);}

export function ConnectorQuery({tool,value,onChange}:{tool:ExtensionTool;value:string;onChange:(value:string)=>void}){
  const properties=tool.inputSchema.properties,required=tool.inputSchema.required;
  let args:Record<string,unknown>={};
  try {const parsed:unknown=JSON.parse(value);if(object(parsed))args=parsed;}catch{/* The JSON editor retains invalid drafts for correction. */}
  const simple=(tool.method==="GET"||tool.method==="MCP")&&object(properties)&&Object.values(properties).every(field=>object(field)&&["string","integer","number","boolean"].includes(String(field.type)));
  if(!simple)return <label>JSON arguments<textarea rows={5} spellCheck={false} value={value} onChange={event=>onChange(event.target.value)}/></label>;
  function update(name:string,text:string,type:unknown){const next={...args};if(text==="")delete next[name];else next[name]=type==="integer"||type==="number"?Number(text):type==="boolean"?text==="true":text;onChange(JSON.stringify(next));}
  return <>{Object.entries(properties).sort(([a],[b])=>{const rank=(name:string)=>name==="q"||name==="query"?0:Array.isArray(required)&&required.includes(name)?1:2;return rank(a)-rank(b)||a.localeCompare(b);}).map(([name,schema])=>{
    if(!object(schema))return null;
    const label=name==="q"||name==="query"?"Search":name==="rows"||name==="per_page"?"Number of results":name.replaceAll("_"," ");
    const isRequired=Array.isArray(required)&&required.includes(name);
    return <label key={name}>{label}{isRequired?"":" (optional)"}{schema.type==="boolean"?<select value={String(args[name]??"")} onChange={event=>update(name,event.target.value,schema.type)} required={isRequired}><option value="">Choose…</option><option value="true">Yes</option><option value="false">No</option></select>:<input type={schema.type==="string"?"text":"number"} value={String(args[name]??"")} required={isRequired} min={typeof schema.minimum==="number"?schema.minimum:undefined} max={typeof schema.maximum==="number"?schema.maximum:undefined} maxLength={typeof schema.maxLength==="number"?schema.maxLength:undefined} step={schema.type==="integer"?1:"any"} onChange={event=>update(name,event.target.value,schema.type)}/>}</label>;
  })}<details><summary>JSON arguments</summary><textarea aria-label="JSON arguments" rows={4} spellCheck={false} value={value} onChange={event=>onChange(event.target.value)}/></details></>;
}

export function ConnectorResult({result}:{result:unknown}){
  const data=object(result)?result:null,items=data&&Array.isArray(data.items)?data.items:data&&object(data.message)&&Array.isArray(data.message.items)?data.message.items:null;
  const records=items?.filter(object).map(item=>{
    const title=typeof item.full_name==="string"?item.full_name:typeof item.title==="string"?item.title:Array.isArray(item.title)&&typeof item.title[0]==="string"?item.title[0]:"Result";
    const link=typeof item.html_url==="string"?item.html_url:typeof item.URL==="string"?item.URL:"";
    let safe="";try{const url=new URL(link);if(url.protocol==="https:"&&!url.username&&!url.password)safe=url.href;}catch{/* Unknown links stay plain text. */}
    return {title,link:safe,description:typeof item.description==="string"?item.description:typeof item.DOI==="string"?`DOI: ${item.DOI}`:""};
  });
  const text=data&&Array.isArray(data.content)?data.content.filter(object).filter(item=>item.type==="text"&&typeof item.text==="string").map(item=>String(item.text)).join("\n\n"):"";
  return <>{text?<div className={styles.mcpText}>{text}</div>:null}{records?<div className={styles.searchResults}>{records.length?records.map((item,index)=><article key={`${item.link}:${index}`}><strong>{item.link?<a href={item.link} target="_blank" rel="noreferrer">{item.title} ↗</a>:item.title}</strong>{item.description?<p>{item.description}</p>:null}</article>):<p>No results returned.</p>}{data?.incomplete_results===true?<p>The provider returned incomplete results. Narrow the search.</p>:null}</div>:null}<details open={!records&&!text}><summary>Raw service response</summary><pre>{JSON.stringify(result,null,2)}</pre></details></>;
}
