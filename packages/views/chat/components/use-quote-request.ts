import { useCallback, useRef, useState } from "react";

/** Hands text from the message list to the composer: `quote` raises a request, the composer consumes it. */
export function useQuoteRequest() {
  const [quoteRequest, setQuoteRequest] = useState<{ id: number; content: string } | null>(null);
  const nextId = useRef(0);
  const quote = useCallback((content: string) => {
    nextId.current += 1;
    setQuoteRequest({ id: nextId.current, content });
  }, []);
  const handleQuoteApplied = useCallback(() => setQuoteRequest(null), []);
  return { quoteRequest, quote, handleQuoteApplied };
}
