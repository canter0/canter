export type PluginInput = {name:string;label:string;help?:string;required:boolean;sensitive:boolean;default?:string};
export type PluginServer = {name:string;url?:string;command?:string;headers?:Record<string,string>};
export type MarketplacePlugin = {slug:string;name:string;description:string;summary:string;source:string;homepage:string;icon?:string;revision:string;servers:PluginServer[];inputs:PluginInput[];notice?:string;steps?:string[]};
export type PluginCatalog = {plugins:MarketplacePlugin[];revision:string;updatedAt:string;cached:boolean;warning?:string};
export type PluginConnection = {id:string;catalog:MarketplacePlugin;state:"installed"|"checking"|"needs_auth"|"review"|"ready"|"failed";failure?:string;revision:number;enabled:boolean};
export type PluginTool = {name:string;remoteName?:string;description:string;impact:"read"|"write"};
export function pluginStatus(connection:PluginConnection) {
  if(!connection.enabled)return "Disabled";
  return {installed:"Finish setup",checking:"Check interrupted — retry",needs_auth:"Sign-in needed",review:"Review access",ready:"Connected",failed:"Connection needs attention"}[connection.state];
}
