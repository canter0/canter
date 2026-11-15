"use client";
import {useCallback,useEffect,useId,useRef,useState,type ReactNode,type KeyboardEvent} from "react";
import {usePopover} from "./use-popover";
import {MotionPresence} from "./motion-presence";
import {WorkspaceIcon} from "./workspace-icon";
import {moveMenuFocus} from "@/lib/interaction";
import styles from "./canter-select.module.css";
export type CanterSelectOption={value:string|number;label:ReactNode;disabled?:boolean};
export function CanterSelect({value,onChange,options,ariaLabel,disabled=false,className,name,required=false}:{value:string|number;onChange:(value:string)=>void;options:CanterSelectOption[];ariaLabel:string;disabled?:boolean;className?:string;name?:string;required?:boolean}){
 const [open,setOpen]=useState(false),[above,setAbove]=useState(false),[height,setHeight]=useState(260),[width,setWidth]=useState(360),[alignRight,setAlignRight]=useState(false),root=useRef<HTMLSpanElement>(null),trigger=useRef<HTMLButtonElement>(null),id=useId(),typed=useRef({text:"",at:0});
 const close=useCallback(()=>setOpen(false),[]),selected=options.find(option=>String(option.value)===String(value));
 usePopover(open&&!disabled,root,close);
 if(disabled&&open)setOpen(false);
 useEffect(()=>{if(!open)return;(root.current?.querySelector<HTMLElement>('[aria-checked="true"]:not(:disabled)')??root.current?.querySelector<HTMLElement>('[role="menuitemradio"]:not(:disabled)'))?.focus();},[open]);
 function show(){if(disabled)return;const bounds=trigger.current?.getBoundingClientRect();if(bounds){let clipTop=0,clipBottom=window.innerHeight,clipLeft=0,clipRight=window.innerWidth;for(let parent=trigger.current?.parentElement;parent;parent=parent.parentElement){const style=getComputedStyle(parent),rect=parent.getBoundingClientRect();if(/auto|scroll|hidden|clip/.test(style.overflowY)){clipTop=Math.max(clipTop,rect.top);clipBottom=Math.min(clipBottom,rect.bottom);}if(/auto|scroll|hidden|clip/.test(style.overflowX)){clipLeft=Math.max(clipLeft,rect.left);clipRight=Math.min(clipRight,rect.right);}}const below=clipBottom-bounds.bottom-8,top=bounds.top-clipTop-8,useAbove=below<Math.min(260,options.length*34+12)&&top>below,right=bounds.left>(clipLeft+clipRight)/2;setAbove(useAbove);setAlignRight(right);setHeight(Math.max(40,Math.min(260,useAbove?top:below)));setWidth(Math.max(bounds.width,Math.min(360,right?bounds.right-clipLeft-8:clipRight-bounds.left-8)));}setOpen(true);}
 function keyboard(event:KeyboardEvent<HTMLElement>){moveMenuFocus(event);if(event.key.length!==1||event.ctrlKey||event.metaKey||event.altKey||event.key===" ")return;const now=Date.now();typed.current={text:(now-typed.current.at<700?typed.current.text:"")+event.key.toLocaleLowerCase(),at:now};const items=[...root.current?.querySelectorAll<HTMLButtonElement>('[role="menuitemradio"]:not(:disabled)')??[]],match=items.find(item=>item.textContent?.trim().toLocaleLowerCase().startsWith(typed.current.text));if(match){event.preventDefault();match.focus();}}
 return <span className={`${styles.root} ${className??""}`} ref={root}>
  {name?<input type="hidden" name={name} value={String(value)} disabled={disabled}/>:null}
  <button ref={trigger} type="button" className={styles.trigger} aria-label={ariaLabel} aria-haspopup="menu" aria-expanded={open&&!disabled} aria-controls={open?id:undefined} disabled={disabled} onClick={()=>open?close():show()} onKeyDown={event=>{if(event.key==="ArrowDown"||event.key==="ArrowUp"){event.preventDefault();show();}}}><span>{selected?.label??"Choose…"}</span><WorkspaceIcon name="down" width="13" height="13"/></button>
  {required?<input className={styles.validation} aria-hidden="true" tabIndex={-1} required value={String(value)} disabled={disabled} onChange={()=>{}} onInvalid={event=>{event.preventDefault();trigger.current?.focus();show();}}/>:null}
  <MotionPresence open={open&&!disabled}><span id={id} role="menu" aria-label={ariaLabel} className={styles.menu} data-above={above} data-align-right={alignRight} style={{maxHeight:height,maxWidth:width}} onKeyDown={keyboard}>{options.map(option=><button type="button" role="menuitemradio" tabIndex={-1} aria-checked={String(option.value)===String(value)} disabled={option.disabled} key={String(option.value)} onClick={()=>{onChange(String(option.value));close();trigger.current?.focus({preventScroll:true});}}><span>{option.label}</span>{String(option.value)===String(value)?<WorkspaceIcon name="check" width="14" height="14"/>:null}</button>)}</span></MotionPresence>
 </span>;
}
