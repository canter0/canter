"use client";
import {useEffect,useRef} from "react";
import {extensionVersionChanges,type WrenExtension} from "@/lib/wren-extensions";
import styles from "./wren-extensions-panel.module.css";

export function WrenExtensionUpdate({current,candidate}:{current:WrenExtension;candidate:WrenExtension}){
 const changes=extensionVersionChanges(current.manifest,candidate.manifest);
 const heading=useRef<HTMLHeadingElement>(null);
 useEffect(()=>{
  // Wait for the enclosing version disclosure to lay out before revealing the diff.
  const frame=requestAnimationFrame(()=>{heading.current?.focus({preventScroll:true});heading.current?.scrollIntoView({block:"start",behavior:"instant"});});
  return()=>cancelAnimationFrame(frame);
 },[current.version,candidate.id,candidate.version]);
 const noToolChanges=!changes.added.length&&!changes.removed.length&&!changes.changed.length;
 return <section className={styles.versionReview} aria-label={`Changes from version ${current.version} to ${candidate.version}`}>
  <h3 ref={heading} tabIndex={-1}>{candidate.manifest.name} · Version {current.version} → {candidate.version}</h3>
  <p>The current version stays in use until you save this update.</p>
  {changes.added.length?<><h4>Added tools</h4><ul>{changes.added.map(tool=><li key={tool.name}><strong>{tool.name.replaceAll("_"," ")}</strong> · {tool.impact==="write"?"Can change data":"Read access"}<p>{tool.description}</p></li>)}</ul></>:null}
  {changes.removed.length?<><h4>Removed tools</h4><ul>{changes.removed.map(tool=><li key={tool.name}>{tool.name.replaceAll("_"," ")}</li>)}</ul></>:null}
  {changes.changed.length?<><h4>Changed tools</h4><ul>{changes.changed.map(tool=><li key={tool.name}><strong>{tool.name.replaceAll("_"," ")}</strong><ul>{tool.changes.map(change=><li key={change}>{change}</li>)}</ul></li>)}</ul></>:null}
  {noToolChanges?<p>No tool definitions changed.</p>:null}
  {changes.authChanged?<p>Authentication: {current.manifest.auth} → {candidate.manifest.auth}. Changing authentication clears saved credentials.</p>:null}
  {changes.originChanged?<p>Service: {current.manifest.origin} → {candidate.manifest.origin}</p>:null}
  {changes.addedEvents.length?<p>Added events: {changes.addedEvents.join(", ")}</p>:null}
  {changes.removedEvents.length?<p>Removed events: {changes.removedEvents.join(", ")}</p>:null}
  {changes.descriptionChanged?<p>The app name or description also changed.</p>:null}
 </section>;
}
