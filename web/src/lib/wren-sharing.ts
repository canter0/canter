import type {Wren} from "./wrens";
export type WrenProfile=Pick<Wren,"name"|"purpose"|"shape"|"palette">&{role:string;section:string};
export type WrenPublication={wrenId:string;workspaceId:string;publisherId:string;publisherName:string;profile:WrenProfile;published:boolean;archived:boolean;revision:number;managerIds:string[];canManage:boolean;conversationId?:string;updatedAt:string};
export type WrenTemplate={id:string;workspaceId:string;authorId:string;profile:WrenProfile;published:boolean;revision:number;updatedAt:string};
export const wrenSharingEvent="canter-wren-sharing-changed";
export function wrenProfile(wren:Wren):WrenProfile{return {name:wren.name,purpose:wren.purpose,role:wren.role??"",section:wren.section??"",shape:wren.shape,palette:wren.palette};}
