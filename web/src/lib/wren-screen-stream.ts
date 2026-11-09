// Length-prefixed JPEG frames. A zero length marks normal bounded renewal;
// truncated/oversized frames are errors, never a successful live connection.
export async function readScreenFrames(body:ReadableStream<Uint8Array>,receive:(frame:Uint8Array)=>Promise<void>){
 const reader=body.getReader();let chunk:Uint8Array=new Uint8Array(0);let offset=0,total=0,count=0;
 async function exact(size:number){
  const out=new Uint8Array(size);let filled=0;
  while(filled<size){
   if(offset===chunk.length){const next=await reader.read();if(next.done)throw new Error("Screen connection interrupted.");chunk=next.value;offset=0;}
   const length=Math.min(size-filled,chunk.length-offset);out.set(chunk.subarray(offset,offset+length),filled);offset+=length;filled+=length;
  }
  return out;
 }
 try{
  while(true){
   const header=await exact(4),size=new DataView(header.buffer).getUint32(0);
   if(size===0)return;
   if(size>2*1024*1024||++count>160||(total+=size)>64*1024*1024)throw new Error("Invalid screen frame size.");
   await receive(await exact(size));
  }
 }finally{await reader.cancel().catch(()=>{});reader.releaseLock();}
}
