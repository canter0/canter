/** Bound the canvas allocation independently of PDF page size or user zoom. */
export function pdfCanvasSize(width:number,height:number,pixelRatio:number){
 if(!Number.isFinite(width)||!Number.isFinite(height)||width<=0||height<=0)throw new Error('This PDF page has unsupported dimensions.');
 const scale=Math.min(Math.max(pixelRatio||1,1),2,Math.sqrt(16_000_000/(width*height)),16_000/width,16_000/height);
 return {width:Math.max(1,Math.floor(width*scale)),height:Math.max(1,Math.floor(height*scale)),scale};
}
export function pdfPreviewError(cause:unknown){
 const name=cause&&typeof cause==='object'&&'name' in cause?cause.name:'';
 if(name==='PasswordException')return 'This PDF is password-protected. Download it to unlock it in your PDF app.';
 if(name==='InvalidPDFException')return 'This file could not be read as a valid PDF. You can download the original.';
 return 'This PDF page could not be rendered. Download the original to open it in your PDF app.';
}
