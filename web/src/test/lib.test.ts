import { formatDuration, formatRelative } from "@/lib/format";
import { can, glob } from "@/lib/permissions";
import { parseSSE } from "@/lib/sse";
import { format } from "@/i18n/i18n";

import { identity } from "./fakeApi";

describe("permessi", () => {
  it("applica i pattern come il server", () => {
    expect(glob("team-a-*", "team-a-api")).toBe(true);
    expect(glob("team-a-*", "team-b-api")).toBe(false);
    expect(glob("prod", "prod-eu")).toBe(false);
    expect(glob("a.b", "aXb")).toBe(false);
  });

  it("concede un'azione solo se un ruolo la copre su applicazione e ambiente", () => {
    const me = identity({
      grants: [{ role: "deployer", actions: ["deployment.submit"], applications: ["shop-*"], environments: ["test"] }],
    });
    expect(can(me, "deployment.submit", "shop-api", "test")).toBe(true);
    expect(can(me, "deployment.submit", "shop-api", "prod")).toBe(false);
    expect(can(me, "deployment.approve", "shop-api", "test")).toBe(false);
    expect(can(identity({ auth_enabled: false, grants: [] }), "deployment.approve", "x", "y")).toBe(true);
    expect(can(undefined, "deployment.read")).toBe(false);
  });
});

describe("SSE", () => {
  it("divide i messaggi e conserva il resto incompleto", () => {
    const { messages, rest } = parseSSE('event: status\ndata: {"a":1}\n\n: keep-alive\n\nid: 7\nevent: log\ndata: [1]\n\nevent: lo');
    expect(messages).toEqual([
      { event: "status", id: undefined, data: '{"a":1}' },
      { event: "log", id: "7", data: "[1]" },
    ]);
    expect(rest).toBe("event: lo");
  });
});

describe("formattazione", () => {
  it("rende durate e tempi relativi", () => {
    expect(formatDuration(450)).toBe("450 ms");
    expect(formatDuration(125_000)).toBe("2 min 5 s");
    expect(formatDuration(3_720_000)).toBe("1 h 2 min");
    expect(formatRelative("2026-10-11T10:00:00Z", "en", Date.parse("2026-10-11T10:05:00Z"))).toBe("5 min. ago");
    expect(format("{a} e {b}", { a: 1 })).toBe("1 e {b}");
  });
});
