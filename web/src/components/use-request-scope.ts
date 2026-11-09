"use client";

import { useCallback, useEffect, useMemo, useRef } from "react";

// Capture once before an async operation, pass to fetch, and check again
// before navigation or starting follow-up work after an await.
export function useRequestScope() {
  const current = useRef<AbortController | null>(null);
  const reset = useCallback(() => {
    current.current?.abort();
    current.current = new AbortController();
  }, []);
  useEffect(() => {
    reset();
    return () => {
      current.current?.abort();
      current.current = null;
    };
  }, [reset]);
  const signal = useCallback(() => current.current?.signal ?? AbortSignal.abort(), []);
  return useMemo(() => ({ signal, reset }), [signal, reset]);
}
