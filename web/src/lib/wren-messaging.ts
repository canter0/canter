import type {KnowledgeScope} from './wren-knowledge';
export type MessageConnection=KnowledgeScope&{id:string;workspaceId:string;ownerId:string;name:string;provider:string;from:string;enabled:boolean;receivingAddress:string;responsibilityId?:string;webhookConfigured:boolean;revision:number;revokedAt?:string;createdAt:string};
export type MessageArtifactRef={id:string;revision:number;sha256:string};
export type MessageContent={to:string[];cc:string[];bcc:string[];subject:string;text:string;replyTo?:string;inReplyToId?:string;attachments:MessageArtifactRef[]};
export type MessageDraft={id:string;connectionId:string;workspaceId:string;conversationId:string;creatorId:string;runId?:string;instructionKey:string;revision:number;content:MessageContent;state:string;providerId?:string;failure?:string;dispatchAt?:string;createdAt:string;updatedAt:string};
export function messageStateLabel(state:string){return ({draft:'Draft',dispatching:'Sending…',accepted:'Accepted by provider',delivered:'Delivered',bounced:'Bounced',complained:'Reported as spam',failed:'Failed',unknown:'Outcome unconfirmed',rejected:'Rejected'} as Record<string,string>)[state]??'Status unavailable';}
export function messageAddresses(text:string):string[]{
 const values=text.split(/[,;\n]+/).map(value=>value.trim()).filter(Boolean);
 if(values.some(value=>!/^[^\s@]+@[^\s@]+$/.test(value)||/[<>\r\n]/.test(value)))throw new Error('Use email addresses without display names, separated by commas or new lines.');
 return values;
}
export function validateMessageRecipients(content:Pick<MessageContent,'to'|'cc'|'bcc'>){
 const values=[...content.to,...content.cc,...content.bcc];
 if(!content.to.length||values.length>20)throw new Error('Choose a To recipient and up to 20 recipients in total.');
 if(new Set(values.map(value=>value.toLowerCase())).size!==values.length)throw new Error('Use each recipient once across To, CC and BCC.');
}
export function messageMayEdit(draft:MessageDraft,connection:MessageConnection|undefined,accountId:string,writer:boolean){return writer&&draft.state==='draft'&&!!connection?.enabled&&!connection.revokedAt&&(draft.creatorId===accountId||connection.ownerId===accountId);}
export function messageMaySend(draft:MessageDraft,connection:MessageConnection|undefined,accountId:string,writer:boolean){return messageMayEdit(draft,connection,accountId,writer)&&connection?.ownerId===accountId;}

export function messageMayReject(draft:MessageDraft,connection:MessageConnection|undefined,accountId:string,writer:boolean){return writer&&draft.state==='draft'&&!!connection&&(draft.creatorId===accountId||connection.ownerId===accountId);}
export type ReceivedMessage={id:string;connectionId:string;conversationId:string;providerId:string;eventId:string;from:string;to:string[];subject:string;messageId:string;inReplyTo?:string;references?:string;threadKey:string;text:string;textAvailable:boolean;htmlPresent:boolean;bodyFormat:string;attachments:{id:string;name:string;mediaType:string;size:number}[]|null;state:string;failure?:string;wakeId?:string;receivedAt:string;createdAt:string};
export function inboxStateLabel(state:string){return ({pending:'Waiting for message body',fetching:'Retrieving message body',ready:'Received',failed:'Body unavailable',blocked:'Connection unavailable'} as Record<string,string>)[state]??'Status unavailable';}
export function normalizedMessageContent(content:MessageContent):MessageContent{return {...content,to:content.to??[],cc:content.cc??[],bcc:content.bcc??[],attachments:content.attachments??[]};}
export function eligibleEmailResponsibilities<T extends {ownerId:string;conversationId:string;state:string;schedule:{kind:string;eventName?:string}}>(values:T[],accountId:string,conversationId:string){return values.filter(value=>value.ownerId===accountId&&value.conversationId===conversationId&&value.state==='active'&&value.schedule.kind==='event'&&value.schedule.eventName==='email_received');}
export function messageWebhookPath(connectionId:string){return `/api/canter/wren-message-webhooks/${encodeURIComponent(connectionId)}`;}
export function replyMessageContent(source:Pick<ReceivedMessage,'id'|'from'|'subject'>):MessageContent{
 const match=/^\s*(?:[^<>]*<([^<>]+)>|([^<>\s]+))\s*$/.exec(source.from);
 const candidate=(match?.[1]??match?.[2]??'').trim();
 let to:string[]=[];
 try{if(candidate)to=messageAddresses(candidate);if(to.length!==1)to=[];}catch{/* The user must explicitly choose a recipient when source formatting is unsupported. */}
 return {to,cc:[],bcc:[],subject:/^re\s*:/i.test(source.subject)?source.subject:`Re: ${source.subject}`,text:'',attachments:[],inReplyToId:source.id};
}
