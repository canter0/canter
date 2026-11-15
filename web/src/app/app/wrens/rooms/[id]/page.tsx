import { RoomConversation } from "@/components/wren-rooms";
import { initialConversation, selectedWorkspace, requireAuthenticated, serverCanterFetch } from "@/lib/server-auth";
import { CanterAPIError } from "@/lib/canter-api";
import type { WrenRoom } from "@/lib/wrens";
import { notFound } from "next/navigation";
export default async function RoomPage({params}:{params:Promise<{id:string}>}) {
 const {id}=await params;const me=await requireAuthenticated();const workspace=await selectedWorkspace(me);let room:WrenRoom;
 try{room=await serverCanterFetch<WrenRoom>(`/workspaces/${encodeURIComponent(workspace.id)}/rooms/${encodeURIComponent(id)}`);}catch(error){if(error instanceof CanterAPIError&&error.status===404)notFound();throw error;}
 const initial=await initialConversation(room.conversationId);return <RoomConversation room={room} initialDetail={initial?.detail??null} initialLoadedAt={initial?.loadedAt}/>;
}
