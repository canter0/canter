import {redirect} from "next/navigation";

export default async function AppsPage({searchParams}:{searchParams:Promise<Record<string,string|string[]|undefined>>}){
 const query=new URLSearchParams();
 for(const [key,value] of Object.entries(await searchParams))for(const item of Array.isArray(value)?value:[value])if(item!==undefined)query.append(key,item);
 const text=query.toString();
 redirect(`/app/customize${text?`?${text}`:""}`);
}
