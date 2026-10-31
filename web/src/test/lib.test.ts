import { formatDuration, formatRelative, tenantLabel } from "@/lib/format";
import { appMatches, can, glob } from "@/lib/permissions";
import { safeReturnPath } from "@/lib/returnPath";
import { parseSSE } from "@/lib/sse";
import { format } from "@/i18n/i18n";

import { identity } from "./fakeApi";

describe("permessi", () => {
  it("applica i pattern qualificati delle applicazioni come il server", () => {
    expect(appMatches("shop", "shop", "untenanted")).toBe(true);
    expect(appMatches("shop", "shop", "payments")).toBe(false);
    expect(appMatches("*", "shop", "payments")).toBe(false);
    expect(appMatches("payments/*", "shop", "payments")).toBe(true);
    expect(appMatches("payments/*", "shop", "retail")).toBe(false);
    expect(appMatches("*/*", "shop", "untenanted")).toBe(true);
    expect(appMatches("payments/*", "payments/shop")).toBe(true);
    const ops = identity({ grants: [{ role: "operator", actions: ["deployment.submit"], applications: ["retail/*"], environments: ["*"] }] });
    expect(can(ops, "deployment.submit", "shop", "prod", "retail")).toBe(true);
    expect(can(ops, "deployment.submit", "shop", "prod", "payments")).toBe(false);
  });

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
  it("nasconde il tenant riservato untenanted", () => {
    expect(tenantLabel("untenanted")).toBeUndefined();
    expect(tenantLabel(undefined)).toBeUndefined();
    expect(tenantLabel("payments")).toBe("payments");
  });

  it("rende durate e tempi relativi", () => {
    expect(formatDuration(450)).toBe("450 ms");
    expect(formatDuration(125_000)).toBe("2 min 5 s");
    expect(formatDuration(3_720_000)).toBe("1 h 2 min");
    expect(formatRelative("2026-10-11T10:00:00Z", "en", Date.parse("2026-10-11T10:05:00Z"))).toBe("5 min. ago");
    expect(format("{a} e {b}", { a: 1 })).toBe("1 e {b}");
  });
});

describe("ritorno dopo il login", () => {
  const origin = "https://manent.example.com";
  it("conserva i percorsi dell'applicazione", () => {
    expect(safeReturnPath("/deployments/d-1?tab=logs#step", origin)).toBe("/deployments/d-1?tab=logs#step");
    expect(safeReturnPath("/", origin)).toBe("/");
  });
  it("rifiuta i percorsi che portano a un altro sito", () => {
    for (const v of ["//evil.com", "/\\evil.com", "/%5Cevil.com", "/%2F/evil.com", "https://evil.com", "evil.com", "/\tx", "/%E0%A4%A", undefined, 42]) {
      expect(safeReturnPath(v, origin)).toBe("/");
    }
  });
});
