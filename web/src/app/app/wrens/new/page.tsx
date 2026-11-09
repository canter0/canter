import { WrensHome } from "@/components/wrens-workspace";
export default async function NewWrenPage({searchParams}:{searchParams:Promise<{section?:string}>}) { const {section}=await searchParams;return <WrensHome create section={section?.slice(0,48)}/>; }
