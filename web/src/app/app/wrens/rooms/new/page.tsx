import { NewRoom } from "@/components/wren-rooms";
export default async function NewRoomPage({ searchParams }: { searchParams: Promise<{ kind?: string;section?:string }> }) {
 const {kind,section}=await searchParams;return <NewRoom kind={kind==="group"?"group":"channel"} section={section?.slice(0,48)}/>;
}
