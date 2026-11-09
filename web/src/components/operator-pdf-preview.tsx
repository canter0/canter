"use client";
import {useEffect,useMemo,useRef,useState} from 'react';
import type {PDFDocumentLoadingTask,PDFDocumentProxy,RenderTask} from 'pdfjs-dist';
import type {OperatorAttachment} from '@/lib/operator-api';
import {attachmentURL} from '@/lib/operator-attachments';
import {pdfCanvasSize,pdfPreviewError} from '@/lib/pdf-preview';
import {WorkspaceIcon} from './workspace-icon';
import styles from './operator-pdf-preview.module.css';
export function OperatorPDFPreview({attachment}:{attachment:OperatorAttachment}){
 const bytes=useMemo(()=>Uint8Array.from(atob(attachment.dataBase64),value=>value.charCodeAt(0)),[attachment.dataBase64]);
 return <PDFBytesPreview bytes={bytes} name={attachment.name} downloadURL={attachmentURL(attachment)}/>;
}
export function PDFBytesPreview({bytes,name,downloadURL}:{bytes:Uint8Array;name:string;downloadURL?:string}){
 const [document,setDocument]=useState<PDFDocumentProxy|null>(null),[error,setError]=useState(''),[page,setPage]=useState(1),[zoom,setZoom]=useState(1),[width,setWidth]=useState(0),[rendered,setRendered]=useState('');
 const container=useRef<HTMLDivElement>(null),canvas=useRef<HTMLCanvasElement>(null);
 const renderKey=`${page}:${zoom}:${width}`;
 useEffect(()=>{
  let disposed=false,loading:PDFDocumentLoadingTask|undefined,passwordBlocked=false;
  async function open(){
   try{
    const pdf=await import('pdfjs-dist/legacy/build/pdf.mjs');if(disposed)return;
    const assets=`/pdfjs/${pdf.version}/`;
    pdf.GlobalWorkerOptions.workerSrc=assets+'pdf.worker.min.mjs';
    const options={data:bytes.slice(),cMapUrl:assets+'cmaps/',standardFontDataUrl:assets+'standard_fonts/',wasmUrl:assets+'wasm/',iccUrl:assets+'iccs/',isEvalSupported:false,enableXfa:false,stopAtErrors:true,disableWorkerRendering:true};
    loading=pdf.getDocument(options);
    loading.onPassword=()=>{passwordBlocked=true;if(!disposed)setError(pdfPreviewError({name:'PasswordException'}));void loading?.destroy();};
    const value=await loading.promise;
    if(disposed){await loading.destroy();return;}setDocument(value);
   }catch(cause){if(!disposed&&!passwordBlocked)setError(pdfPreviewError(cause));}
  }
  void open();return()=>{disposed=true;void loading?.destroy();};
 },[bytes]);
 useEffect(()=>{
  if(!container.current)return;
  const observer=new ResizeObserver(entries=>{const next=Math.floor(entries[0]?.contentRect.width??0);setWidth(value=>value===next?value:next);});
  observer.observe(container.current);return()=>observer.disconnect();
 },[]);
 useEffect(()=>{
  if(!document||!width||!canvas.current)return;
  const target=canvas.current;let disposed=false,render:RenderTask|undefined;
  async function draw(){
   try{
    const value=await document!.getPage(page);if(disposed)return;
    const original=value.getViewport({scale:1});
    const viewport=value.getViewport({scale:Math.max(1,width-28)/original.width*zoom});
    const pixels=pdfCanvasSize(viewport.width,viewport.height,window.devicePixelRatio);
    target.width=pixels.width;target.height=pixels.height;target.style.width=`${viewport.width}px`;target.style.height=`${viewport.height}px`;
    const context=target.getContext('2d');if(!context)throw new Error('Canvas unavailable');
    render=value.render({canvas:target,canvasContext:context,viewport,transform:[pixels.scale,0,0,pixels.scale,0,0],background:'#ffffff'});
    await render.promise;if(!disposed)setRendered(renderKey);
   }catch(cause){if(!disposed)setError(pdfPreviewError(cause));}
  }
  void draw();return()=>{disposed=true;render?.cancel();};
 },[document,page,width,zoom,renderKey]);
 return <section className={styles.preview} aria-label="PDF preview"><div className={styles.tools}><div className={styles.group}><button type="button" aria-label="Previous PDF page" disabled={!document||page<=1} onClick={()=>setPage(value=>value-1)}><WorkspaceIcon name="chevron" width="13" height="13" style={{transform:'rotate(180deg)'}}/></button><span>{document?`Page ${page} of ${document.numPages}`:'PDF'}</span><button type="button" aria-label="Next PDF page" disabled={!document||page>=document.numPages} onClick={()=>setPage(value=>value+1)}><WorkspaceIcon name="chevron" width="13" height="13"/></button></div><div className={styles.group}><button type="button" aria-label="Zoom PDF out" disabled={!document||zoom<=.75} onClick={()=>setZoom(value=>Math.max(.75,value-.25))}>−</button><button type="button" aria-label="Fit PDF to width" disabled={!document} onClick={()=>setZoom(1)}>{Math.round(zoom*100)}%</button><button type="button" aria-label="Zoom PDF in" disabled={!document||zoom>=2} onClick={()=>setZoom(value=>Math.min(2,value+.25))}>+</button>{downloadURL?<a href={downloadURL} download={name}>Download</a>:null}</div></div>{error?<p className={styles.message} role="alert">{error}</p>:<>{!document||rendered!==renderKey?<p className={styles.loading} role="status">{document?'Rendering page…':'Loading PDF…'}</p>:null}<div ref={container} className={styles.stage}><canvas key={renderKey} ref={canvas} role="img" aria-label={`Page ${page} of ${name}`} style={{visibility:rendered===renderKey?'visible':'hidden'}}/></div></>}</section>;
}
