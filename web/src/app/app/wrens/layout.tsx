import {WrenNavigation} from "@/components/wren-navigation";
import {WrenCreationModals} from "@/components/wren-creation-modals";
import type { ReactNode } from "react";
import { WrensProvider } from "@/components/wrens-context";
export default function WrensLayout({ children }: { children: ReactNode }) { return <WrensProvider>{children}<WrenNavigation/><WrenCreationModals/></WrensProvider>; }
