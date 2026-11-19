import {canterFetch} from "./canter-api";
import type {ExtensionScope,ExtensionTarget} from "./wren-knowledge";
import type {ConnectorCatalogEntry,ExtensionInstallation,WrenExtension} from "./wren-extensions";

export async function addCatalogConnector(base:string,target:ExtensionTarget,entry:ConnectorCatalogEntry,id:string){
 const extension=await canterFetch<WrenExtension>(`${base}/catalog/${encodeURIComponent(entry.slug)}/prepare`,{method:"POST"});
 return canterFetch<ExtensionInstallation>(`${base}/installations`,{method:"POST",body:JSON.stringify({id,...target.scope,extensionId:extension.id,version:extension.version,expectedRevision:0,enabled:true,allowWrite:false,credentialMode:"personal"})});
}

export async function setInstallationEnabled(base:string,record:ExtensionInstallation,scope:ExtensionScope,enabled:boolean){
 return canterFetch<ExtensionInstallation>(`${base}/installations/${encodeURIComponent(record.id)}`,{method:"PATCH",body:JSON.stringify({id:record.id,...scope,extensionId:record.extensionId,version:record.version,expectedRevision:record.revision,enabled,allowWrite:record.allowWrite,credentialMode:record.credentialMode})});
}
