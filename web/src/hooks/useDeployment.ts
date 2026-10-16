import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { TERMINAL, unwrap, type LogLine, type Status } from "@/api/client";
import { useApi } from "@/api/context";
import { useAuth } from "@/auth/auth";
import { stream } from "@/lib/sse";

export type LiveMode = "stream" | "polling";

/**
 * useDeployment follows a deploy: state and log lines arrive from the SSE stream
 * (/deployments/:id/events); if the stream is unavailable (e.g. a proxy blocking it) it
 * falls back to reading state and logs periodically.
 */
export function useDeployment(id: string) {
  const api = useApi();
  const auth = useAuth();
  const qc = useQueryClient();
  const [lines, setLines] = useState<LogLine[]>([]);
  const [mode, setMode] = useState<LiveMode>("stream");
  const offset = useRef(0);

  const status = useQuery({
    queryKey: ["deployment", id],
    queryFn: async (): Promise<Status> => unwrap(await api.GET("/api/v1/deployments/{id}", { params: { path: { id } } })),
    refetchInterval: (q) => (mode === "polling" && q.state.data && !TERMINAL.has(q.state.data.state) ? 3000 : false),
  });
  const known = status.isSuccess;
  const done = status.data ? TERMINAL.has(status.data.state) : false;

  // Stream: starts when the deploy exists; when the deploy ends it sends the remaining lines and "end".
  useEffect(() => {
    if (!known || mode !== "stream") return;
    const ctrl = new AbortController();
    void stream({
      url: `/api/v1/deployments/${encodeURIComponent(id)}/events?offset=${offset.current}`,
      token: auth.token,
      signal: ctrl.signal,
      onMessage: (m) => {
        if (m.event === "status") {
          const st = JSON.parse(m.data) as Status;
          qc.setQueryData(["deployment", id], st);
          if (TERMINAL.has(st.state)) {
            void qc.invalidateQueries({ queryKey: ["steps", id] });
          }
        } else if (m.event === "log") {
          const batch = JSON.parse(m.data) as LogLine[];
          if (m.id) offset.current = Number(m.id);
          setLines((ls) => [...ls, ...batch]);
        }
      },
      onUnavailable: () => setMode("polling"),
    });
    return () => ctrl.abort();
  }, [id, known, mode, auth.token, qc]);

  // Fallback: new lines periodically until the deploy ends.
  useEffect(() => {
    if (mode !== "polling") return;
    let stop = false;
    const tick = async () => {
      const page = unwrap(
        await api.GET("/api/v1/deployments/{id}/logs", { params: { path: { id }, query: { offset: offset.current } } }),
      );
      if (stop) return;
      if (page.lines.length > 0) {
        offset.current = page.next;
        setLines((ls) => [...ls, ...page.lines]);
      }
      if (!page.done) timer = setTimeout(() => void tick().catch(() => undefined), 3000);
    };
    let timer = setTimeout(() => void tick().catch(() => undefined), 0);
    return () => {
      stop = true;
      clearTimeout(timer);
    };
  }, [api, id, mode]);

  const steps = useQuery({
    queryKey: ["steps", id],
    queryFn: async () => unwrap(await api.GET("/api/v1/deployments/{id}/steps", { params: { path: { id } } })).steps,
    enabled: known,
    refetchInterval: done ? false : 5000,
  });

  return { status, steps, lines, mode };
}
