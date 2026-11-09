"use client";
import type { OperatorSurface } from "@/lib/operator-api";
import { activeJob, jobLabel, useWrenResource, workBase, type WrenJobRecord, type WrenArtifactRecord } from "@/lib/wren-work";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./wren-work-overview.module.css";

const jobsActive = (value: {jobs: WrenJobRecord[]}) => value.jobs.some(activeJob);

export function WrenWorkOverview({workspaceId,conversationId,name,onSelect}:{workspaceId:string;conversationId:string;name:string;onSelect:(surface:OperatorSurface,label?:string)=>void}) {
  const base=workBase(workspaceId),query=`?conversationId=${encodeURIComponent(conversationId)}`;
  const jobs=useWrenResource<{jobs:WrenJobRecord[]}>(`${base}/wren-jobs${query}`,5000,jobsActive);
  const files=useWrenResource<{artifacts:WrenArtifactRecord[]}>(`${base}/wren-artifacts${query}`);
  return <div className={styles.overview}>
    <header className={styles.heading}><p>{name}</p><h2 tabIndex={-1}>Work from this conversation</h2><span>Follow delegated tasks and open the results.</span></header>
    <nav className={styles.destinations} aria-label="Work collections">{([['wren-skills','Skills','file'],['wren-responsibility','Responsibilities','activity'],['wren-projects','Projects','folder'],['wren-library','File library','file'],['wren-messaging','Messages','message']] as const).map(([kind,label,icon])=><button key={kind} onClick={()=>onSelect({kind})}><WorkspaceIcon name={icon} width="17" height="17"/>{label}<WorkspaceIcon name="right" width="13" height="13"/></button>)}</nav>
    <div className={styles.columns}>
      <section><div className={styles.sectionTitle}><h3>Delegated work</h3>{jobs.data?<span>{jobs.data.jobs.length}</span>:null}</div>
        {jobs.loading?<p role="status" className={styles.note}>Loading work…</p>:null}{jobs.error?<p role="alert" className={styles.note}>{jobs.data ? "Couldn’t refresh work. Showing the last loaded tasks." : "Couldn’t load work."} <button onClick={jobs.refresh}>Retry</button></p>:null}
        {jobs.data?.jobs.length===0?<p className={styles.empty}>When you delegate work in this conversation, you can follow it here.</p>:null}
        {jobs.data?.jobs.map(job=><button className={styles.job} key={job.id} onClick={()=>onSelect({kind:'wren-job',id:job.id},job.title)} title="Open task"><span className={styles.dot} data-state={job.status}/><span><strong>{job.title}</strong><small>{jobLabel(job)}</small></span><WorkspaceIcon name="right" width="14" height="14"/></button>)}
      </section>
      <section><div className={styles.sectionTitle}><h3>Files from this conversation</h3>{files.data?<span>{files.data.artifacts.length}</span>:null}</div>
        {files.loading?<p role="status" className={styles.note}>Loading files…</p>:null}{files.error?<p role="alert" className={styles.note}>{files.data ? "Couldn’t refresh files. Showing the last loaded files." : "Couldn’t load files."} <button onClick={files.refresh}>Retry</button></p>:null}
        {files.data?.artifacts.length===0?<p className={styles.empty}>Saved results will appear here, ready to open here.</p>:null}
        {files.data?.artifacts.map(file=><button key={file.id} className={styles.file} onClick={()=>onSelect({kind:'wren-artifact',id:file.id},file.name)}><span className={styles.fileIcon}><WorkspaceIcon name="file" width="20" height="20"/></span><span><strong>{file.name}</strong><small>Version {file.revision} · {file.mediaType.replace('text/','')}</small></span><WorkspaceIcon name="right" width="14" height="14"/></button>)}
      </section>
    </div>
  </div>;
}
