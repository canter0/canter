import type {Wren,WrenRoom} from './wrens';
export type WrenProject={id:string;name:string;systemName?:string;revision:number;archived:boolean;createdAt:string;updatedAt:string};
export type ProjectNote={id:string;projectId:string;title:string;content:string;revision:number;authorId?:string;sources:{urls?:string[]};deleted:boolean;createdAt:string;updatedAt:string};
export type ProjectGrant={id:string;projectId:string;conversationId:string;targetKind:'private'|'room';wrenId?:string;ownerId?:string;revision:number;revoked:boolean;canManage:boolean;createdAt:string;updatedAt:string};
export function projectConversationTarget(conversationId:string|undefined,accountId:string,wrens:Wren[],rooms:WrenRoom[]){
 const room=rooms.find(value=>value.conversationId===conversationId);
 if(room)return {id:room.conversationId,label:room.kind==='channel'?`Channel ${room.name.replace(/^#\s*/, '')}`:`Group chat ${room.name}`,canManage:room.ownerId===accountId,shared:true};
 const wren=wrens.find(value=>value.conversationId===conversationId);
 return wren?{id:wren.conversationId,label:`Your private chat with ${wren.name}`,canManage:true,shared:false}:null;
}
export function projectSourceURLs(text:string):string[]{
 const urls=text.split(/\r?\n/).map(value=>value.trim()).filter(Boolean);
 if(urls.length>12)throw new Error('Use up to 12 source URLs.');
 return [...new Set(urls.map(value=>{let url:URL;try{url=new URL(value);}catch{throw new Error('Use complete HTTP or HTTPS source URLs.');}if(!['http:','https:'].includes(url.protocol)||url.username||url.password)throw new Error('Use HTTP or HTTPS source URLs without credentials.');return value;}))];
}
