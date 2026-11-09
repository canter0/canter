import type {WrenArtifactRecord} from "./wren-work";
export type WrenLibraryEntry=WrenArtifactRecord&{authorName:string;sourceWrenName?:string;conversationTitle:string;pinned:boolean;pinnedAt?:string};
export type WrenLibraryPage={artifacts:WrenLibraryEntry[];hasMore:boolean;nextCursor?:string};
export type WrenArtifactDocument={artifact:WrenArtifactRecord;kind:"text"|"csv"|"unsupported";editable:boolean;reason?:string;text?:string;rows?:string[][];delimiter?:","|"\t";lineEnding?:"lf"|"crlf";hasBOM?:boolean};
export function artifactDraftProblem(document:WrenArtifactDocument,text:string,rows:string[][]){
 if(document.kind==="text")return new TextEncoder().encode(text).length>1048576?"Keep this file under 1 MiB to edit it here.":"";
 if(document.kind!=="csv")return "This format can be downloaded and opened in its application.";
 if(rows.length>2000||rows.some(row=>row.length>64)||rows.reduce((total,row)=>total+row.length,0)>50000)return "The editor supports up to 2,000 rows, 64 columns and 50,000 cells.";
 const delimiter=document.delimiter??",",eol=document.lineEnding==="crlf"?"\r\n":"\n";
 const value=(document.hasBOM?"\uFEFF":"")+rows.map(row=>row.map(cell=>/["\r\n]/.test(cell)||cell.includes(delimiter)?`"${cell.replaceAll('"','""')}"`:cell).join(delimiter)).join(eol)+eol;
 return new TextEncoder().encode(value).length>1048576?"Keep this file under 1 MiB to edit it here.":"";
}
export function mergeLibraryPages(current:WrenLibraryEntry[],incoming:WrenLibraryEntry[]){return [...new Map([...current,...incoming].map(entry=>[entry.id,entry])).values()];}
