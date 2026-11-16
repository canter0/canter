"use client";

import { createContext } from "react";
import type { OperatorSurface } from "@/lib/operator-api";

/** Settings use the same work navigation as the conversation toolbar. */
export const WrenWorkNavigation = createContext<((surface: OperatorSurface) => void) | null>(null);
