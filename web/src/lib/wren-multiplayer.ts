export type WrenMember = { accountId:string;email:string;role:"owner"|"operator"|"viewer" };
export type WrenInvite = { id:string;workspaceId:string;inviterId:string;email:string;role:"operator"|"viewer";state:"pending"|"expired"|"accepted"|"declined"|"revoked";expiresAt:string;createdAt:string };
export type WrenSearchResult = {kind:"message"|"artifact"|"job";id:string;conversationId:string;title:string;excerpt:string;messageId?:string;runId?:string;revision?:number;updatedAt:string};
export function canMarkWrenRead(value:{visible:boolean;focused:boolean;covered:boolean;gap:number;sequence:number}) {return value.visible&&value.focused&&!value.covered&&value.gap<=48&&value.gap>=-1&&Number.isSafeInteger(value.sequence)&&value.sequence>0;}
export function wrenSearchBytes(query:string){return new TextEncoder().encode(query.trim()).length;}
export function wrenSearchDestination(result:WrenSearchResult,wrens:{id:string;conversationId:string}[],rooms:{id:string;conversationId:string}[]) {
 const wren=wrens.find(item=>item.conversationId===result.conversationId),room=rooms.find(item=>item.conversationId===result.conversationId);
 const base=room?`/app/wrens/rooms/${encodeURIComponent(room.id)}`:wren?`/app/wrens/${encodeURIComponent(wren.id)}`:null;
 if(!base)return null;
 const query=new URLSearchParams(result.kind==="message"?{message:result.messageId||result.id}:{work:result.kind==="artifact"?"wren-artifact":"wren-job",workId:result.id});
 return `${base}?${query}`;
}
