import test from 'node:test';
import assert from 'node:assert/strict';
import {pdfCanvasSize,pdfPreviewError} from '../src/lib/pdf-preview.ts';
test('PDF canvas rendering bounds unusually large pages and display scale without allocating unbounded pixels',()=>{
 const normal=pdfCanvasSize(800,1000,2);assert.deepEqual(normal,{width:1600,height:2000,scale:2});
 for(const dimensions of [[100000,100000,4],[1e8,10,2],[1000,2000,3]]){
  const value=pdfCanvasSize(...dimensions);assert.ok(value.width*value.height<=16000000);assert.ok(value.width<=16000);assert.ok(value.height<=16000);
 }
 assert.throws(()=>pdfCanvasSize(Infinity,100,1));assert.throws(()=>pdfCanvasSize(0,10,1));
});
test('password and unsupported PDFs have truthful human-readable fallback without exposing raw parser errors',()=>{
 assert.match(pdfPreviewError({name:'PasswordException',message:'secret'}),/password-protected/);
 assert.match(pdfPreviewError({name:'InvalidPDFException'}),/valid PDF/);
 assert.doesNotMatch(pdfPreviewError(new Error('private parser internals')),/private parser/);
});
