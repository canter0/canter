import type {ConnectorCatalogEntry,ExtensionInstallation} from "./wren-extensions";

export function appsPageHref(conversationId?:string,open?:{installation?:string;source?:string;app?:"github"|"linear"}){
 const query=new URLSearchParams();
 if(conversationId)query.set("conversation",conversationId);
 if(open?.installation)query.set("installation",open.installation);
 if(open?.source)query.set("source",open.source);
 if(open?.app)query.set("app",open.app);
 const text=query.toString();
 return `/app/customize${text?`?${text}`:""}`;
}

export function appProvider(value:{name:string;origin?:string;tools?:{endpoint:string}[]}):"github"|"linear"|null{
 const text=`${value.name} ${value.origin??""} ${(value.tools??[]).map(tool=>tool.endpoint).join(" ")}`.toLowerCase();
 if(text.includes("linear"))return "linear";
 if(text.includes("github"))return "github";
 return null;
}

export function appMonogram(name:string){
 const initials=name.split(/\s+/).filter(Boolean).map(word=>word[0]).join("");
 return (initials.length>1?initials:name.replace(/[^A-Za-z0-9]/g,"")).slice(0,2).toUpperCase();
}

export function installedSource(installations:ExtensionInstallation[],entry:ConnectorCatalogEntry){
 return installations.find(item=>!item.revokedAt&&item.extensionId.startsWith("extension_catalog_")&&item.extension.manifest.origin===entry.manifest.origin&&item.extension.manifest.name===entry.manifest.name);
}
