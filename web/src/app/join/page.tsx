import type {Metadata} from "next";
import {WrenJoin} from "@/components/wren-join";
export const metadata:Metadata={title:"Join organization",robots:{index:false,follow:false},referrer:"no-referrer"};
export default async function JoinPage({searchParams}:{searchParams:Promise<{invite?:string}>}){const {invite=""}=await searchParams;return <WrenJoin token={invite}/>;}
