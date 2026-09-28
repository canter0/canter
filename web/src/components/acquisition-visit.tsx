"use client";

import { useEffect } from "react";
import { recordAcquisition } from "@/lib/acquisition";

export function AcquisitionVisit({ landingPath }: { landingPath: "/" | "/pricing" }) {
  useEffect(() => { void recordAcquisition(landingPath); }, [landingPath]);
  return null;
}
