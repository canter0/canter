import {NextResponse,type NextRequest} from "next/server";
import {serverCanterFetch,selectedWorkspace} from "@/lib/server-auth";
import {CanterAPIError,type Me} from "@/lib/canter-api";
import {workspacePreference,workspacePreferenceCookie} from "@/lib/workspace-selection";
function failure(cause:unknown){return NextResponse.json({error:cause instanceof CanterAPIError&&cause.status===401?"Sign in to continue.":"Your organization membership could not be verified."},{status:cause instanceof CanterAPIError?cause.status:503,headers:{"Cache-Control":"no-store"}});}
export async function GET(){try{const me=await serverCanterFetch<Me>("/me"),workspace=await selectedWorkspace(me);return NextResponse.json({...me,workspace},{headers:{"Cache-Control":"no-store"}});}catch(cause){return failure(cause);}}
export async function POST(request:NextRequest){
 let origin:URL;try{origin=new URL(request.headers.get("origin")??"");}catch{return NextResponse.json({error:"Use this organization switch from Canter."},{status:403});}
 if(!["http:","https:"].includes(origin.protocol)||origin.host!==request.headers.get("host"))return NextResponse.json({error:"Use this organization switch from Canter."},{status:403});
 try{const text=await request.text();if(text.length>4096)return NextResponse.json({error:"Invalid organization selection."},{status:413});let body:unknown;try{body=JSON.parse(text);}catch{return NextResponse.json({error:"Invalid organization selection."},{status:400});}
  const id=body&&typeof body==="object"&&"workspaceId" in body?body.workspaceId:null;if(typeof id!=="string"||!id)return NextResponse.json({error:"Choose an organization."},{status:400});
  const me=await serverCanterFetch<Me>("/me"),workspace=me.workspaces.find(item=>item.id===id);if(!workspace)return NextResponse.json({error:"You no longer have access to this organization."},{status:403});
  const response=NextResponse.json({workspaceId:workspace.id,accountId:me.account.id},{headers:{"Cache-Control":"no-store"}});response.cookies.set(workspacePreferenceCookie,workspacePreference(me.account.id,workspace.id),{path:"/",httpOnly:true,sameSite:"lax",secure:origin.protocol==="https:",maxAge:31536000});return response;
 }catch(cause){return failure(cause);}
}
