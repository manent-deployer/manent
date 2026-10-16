// SSE client on fetch: EventSource does not allow the Authorization header, needed with OIDC.
// It resumes after a disconnection with Last-Event-ID (the log offset).

export interface SSEMessage {
  event: string;
  id?: string;
  data: string;
}

/** parseSSE splits the received text into complete messages and returns the incomplete rest. */
export function parseSSE(buffer: string): { messages: SSEMessage[]; rest: string } {
  const messages: SSEMessage[] = [];
  const blocks = buffer.replace(/\r\n/g, "\n").split("\n\n");
  const rest = blocks.pop() ?? "";
  for (const block of blocks) {
    let event = "message";
    let id: string | undefined;
    const data: string[] = [];
    for (const line of block.split("\n")) {
      if (line === "" || line.startsWith(":")) continue;
      const i = line.indexOf(":");
      const field = i < 0 ? line : line.slice(0, i);
      const value = i < 0 ? "" : line.slice(i + 1).replace(/^ /, "");
      if (field === "event") event = value;
      else if (field === "id") id = value;
      else if (field === "data") data.push(value);
    }
    if (data.length > 0 || event !== "message") messages.push({ event, id, data: data.join("\n") });
  }
  return { messages, rest };
}

export interface StreamOptions {
  url: string;
  token?: () => string | undefined;
  onMessage: (m: SSEMessage) => void;
  /** Called when the stream is unavailable (e.g. a proxy blocking it). */
  onUnavailable?: (err: unknown) => void;
  signal: AbortSignal;
}

/** stream reads the SSE endpoint until the server closes after "end" or the signal aborts. */
export async function stream({ url, token, onMessage, onUnavailable, signal }: StreamOptions): Promise<void> {
  let lastId: string | undefined;
  let failures = 0;
  while (!signal.aborted) {
    let ended = false;
    try {
      const headers: Record<string, string> = { Accept: "text/event-stream" };
      const t = token?.();
      if (t) headers.Authorization = `Bearer ${t}`;
      if (lastId) headers["Last-Event-ID"] = lastId;
      const res = await fetch(url, { headers, signal, cache: "no-store" });
      if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
      failures = 0;
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        const parsed = parseSSE(buf + value);
        buf = parsed.rest;
        for (const m of parsed.messages) {
          if (m.id) lastId = m.id;
          if (m.event === "end") ended = true;
          onMessage(m);
        }
      }
    } catch (err) {
      if (signal.aborted) return;
      failures++;
      if (failures >= 3) {
        onUnavailable?.(err);
        return;
      }
    }
    if (ended) return;
    await new Promise((r) => setTimeout(r, Math.min(1000 * 2 ** failures, 10000)));
  }
}
