import type {KnowledgeScope} from "./wren-knowledge";
export type ExtensionTool={name:string;description:string;endpoint:string;method?:"GET"|"MCP";remoteName?:string;impact:"read"|"write";inputSchema:Record<string,unknown>;outputSchema?:Record<string,unknown>|boolean};
export type ExtensionManifest={name:string;description:string;origin:string;auth:"none"|"bearer";tools:ExtensionTool[];events:string[]};
export type WrenExtension={id:string;workspaceId:string;ownerId:string;authorId:string;version:number;latestVersion:number;disabled:boolean;manifest:ExtensionManifest;sha256:string;createdAt:string;updatedAt:string};
export type ExtensionInstallation=KnowledgeScope&{id:string;workspaceId:string;ownerId:string;extensionId:string;version:number;revision:number;enabled:boolean;revokedAt?:string;allowWrite:boolean;credentialMode:"personal"|"service";credentialConfigured:boolean;appAccountId?:string;credentialRevision:number;extension:WrenExtension;createdAt:string;updatedAt:string};
export type ExtensionCall={id:string;installationId:string;accountId:string;runId?:string;operationKey:string;toolName:string;version:number;status:"completed"|"failed"|"unknown"|"running";httpStatus:number;result?:unknown;failure?:string;private:boolean;createdAt:string;completedAt?:string};
export type ExtensionEventBinding={id:string;installationId:string;ownerId:string;responsibilityId:string;eventName:string;revision:number;enabled:boolean;lastEventAt?:string;createdAt:string;updatedAt:string};
function object(value:unknown):value is Record<string,unknown>{return !!value&&typeof value==="object"&&!Array.isArray(value);}
export function parseExtensionArguments(text:string):Record<string,unknown>{if(new TextEncoder().encode(text).length>65536)throw new Error("Keep test arguments under 64 KiB.");const value:unknown=JSON.parse(text);if(!object(value))throw new Error("Tool arguments must be a JSON object.");return value;}
export function parseExtensionManifest(text:string):ExtensionManifest{
 if(new TextEncoder().encode(text).length>81920)throw new Error("Keep the manifest under 80 KiB.");const value:unknown=JSON.parse(text);
 if(!object(value)||typeof value.name!=="string"||typeof value.description!=="string"||typeof value.origin!=="string"||(value.auth!=="none"&&value.auth!=="bearer")||!Array.isArray(value.tools)||(value.events!==undefined&&!Array.isArray(value.events)))throw new Error("Include name, description, origin, auth, tools and events in the manifest.");
 const tools:ExtensionTool[]=value.tools.map(tool=>{if(!object(tool)||typeof tool.name!=="string"||typeof tool.description!=="string"||typeof tool.endpoint!=="string"||(tool.impact!=="read"&&tool.impact!=="write")||!object(tool.inputSchema)||tool.inputSchema.type!=="object"||(tool.method!==undefined&&tool.method!=="GET"&&tool.method!=="MCP")||(tool.remoteName!==undefined&&(tool.method!=="MCP"||typeof tool.remoteName!=="string"))||(tool.method==="GET"&&tool.impact!=="read")||(tool.outputSchema!==undefined&&!object(tool.outputSchema)&&typeof tool.outputSchema!=="boolean"))throw new Error("Each tool needs a name, description, endpoint, read/write impact and object input schema.");return {name:tool.name,description:tool.description,endpoint:tool.endpoint,impact:tool.impact,inputSchema:tool.inputSchema,...(tool.method==="GET"||tool.method==="MCP")?{method:tool.method}:{},...typeof tool.remoteName==="string"?{remoteName:tool.remoteName}:{},...tool.outputSchema!==undefined?{outputSchema:tool.outputSchema}:{}};});
 const events:string[]=(value.events??[]).map(event=>{if(typeof event!=="string")throw new Error("Event names must be strings.");return event;});
 return {name:value.name,description:value.description,origin:value.origin,auth:value.auth,tools,events};
}
export function extensionInstallationLabel(value:ExtensionInstallation){return value.revokedAt?"Revoked":value.extension.disabled?"Disabled by publisher":!value.enabled?"Paused here":value.extension.manifest?.auth==="bearer"&&!value.credentialConfigured?"Sign-in needed":"Enabled here";}
export function extensionCallLabel(value:ExtensionCall){return value.status==="unknown"?"Outcome unknown":value.status==="running"?"Awaiting outcome":value.status==="completed"?"Completed":"Failed";}

export type ConnectorCatalogEntry={slug:string;category:string;website:string;documentation:string;limitation:string;example:Record<string,unknown>;manifest:ExtensionManifest};

export function extensionToolSummary(tool:ExtensionTool):string {
 if(tool.method==="GET"){
  if(tool.endpoint==="https://api.github.com/search/repositories")return "Find public repositories by name, topic, language, or owner. Results include links and descriptions.";
  if(tool.endpoint==="https://api.github.com/search/issues")return "Find public issues and pull requests, including their status and source links.";
  if(tool.endpoint==="https://api.crossref.org/works")return "Look up scholarly titles, authors, publication details, and DOI links. This searches metadata, not the full text.";
 }
 return tool.description;
}

export type WrenAppAccount={id:string;provider:string;endpoint:string;label:string;revision:number;reconnect:boolean;createdAt:string};

function canonical(value:unknown):string {
 if(Array.isArray(value))return `[${value.map(canonical).join(",")}]`;
 if(object(value))return `{${Object.keys(value).sort().map(key=>`${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
 return JSON.stringify(value)??"undefined";
}
export function extensionVersionChanges(before:ExtensionManifest,after:ExtensionManifest){
 const prior=new Map(before.tools.map(tool=>[tool.name,tool])),next=new Map(after.tools.map(tool=>[tool.name,tool]));
 const added=after.tools.filter(tool=>!prior.has(tool.name)),removed=before.tools.filter(tool=>!next.has(tool.name));
 const changed=after.tools.flatMap(tool=>{
  const old=prior.get(tool.name);if(!old)return [];
  const changes:string[]=[];
  if(old.impact!==tool.impact)changes.push(`${old.impact==="write"?"Write":"Read"} access → ${tool.impact==="write"?"write":"read"} access`);
  if(old.endpoint!==tool.endpoint)changes.push(`Destination: ${old.endpoint} → ${tool.endpoint}`);
  if((old.method??"POST")!==(tool.method??"POST"))changes.push(`Transport: ${old.method??"POST"} → ${tool.method??"POST"}`);
  if(old.remoteName!==tool.remoteName)changes.push("Remote tool identity changed");
  if(canonical(old.inputSchema)!==canonical(tool.inputSchema))changes.push("Accepted inputs changed");
  if(canonical(old.outputSchema)!==canonical(tool.outputSchema))changes.push("Returned data schema changed");
  if(old.description!==tool.description)changes.push("Tool instructions changed");
  return changes.length?[{name:tool.name,changes}]:[];
 });
 return {added,removed,changed,authChanged:before.auth!==after.auth,originChanged:before.origin!==after.origin,
  addedEvents:after.events.filter(event=>!before.events.includes(event)),removedEvents:before.events.filter(event=>!after.events.includes(event)),
  descriptionChanged:before.name!==after.name||before.description!==after.description};
}
