import {WrenAppsPage} from "@/components/wren-apps-page";
import {requireAuthenticated} from "@/lib/server-auth";

export default async function CustomizePage({searchParams}:{searchParams:Promise<{conversation?:string;installation?:string;source?:string;app?:string;github?:string}>}){
 const query=await searchParams;
 await requireAuthenticated();
 return <WrenAppsPage conversationId={query.conversation} installationId={query.installation} catalogSlug={query.source} app={query.app} githubResult={query.github}/>;
}
