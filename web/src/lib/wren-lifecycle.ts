export type WrenLifecycle={conversationId:string;kind:"wren"|"room"|"chat";name:string;hidden:boolean;archived:boolean;revision:number;archivedAt?:string;archivedBy?:string;canManage:boolean};
export const wrenLifecycleEvent="canter-wren-lifecycle-changed";
export function lifecycleVisible(state:WrenLifecycle|undefined,view:string){
 if(view==="all")return true;
 if(view==="hidden")return !!state?.hidden;
 if(view==="archived")return !!state?.archived;
 return !state?.hidden&&!state?.archived;
}
