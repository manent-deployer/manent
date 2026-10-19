import { QueryClient } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { AppWithConfig } from "@/App";
import type { Api } from "@/api/client";
import type { Lang } from "@/i18n/i18n";
import type { UIConfig } from "@/lib/config";

import { dep, fakeApi, identity, now } from "./fakeApi";

const config: UIConfig = { auth_mode: "none", version: "1.2.3" };

function renderApp(api: Api, path: string, lang: Lang = "en") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={[path]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
      <AppWithConfig config={config} api={api} queryClient={qc} lang={lang} />
    </MemoryRouter>,
  );
}

describe("deploy", () => {
  it("elenca i deploy con stato leggibile e link al dettaglio", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity({ auth_enabled: false }) };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/deployments")
        return { json: { deployments: [dep({ id: "a", state: "ERROR" }), dep({ id: "b", state: "PENDING", rollout_progress: "1/3" })] } };
    });
    renderApp(api, "/deployments");
    const table = await screen.findByRole("table", { name: "Deployments, newest first" });
    expect(within(table).getAllByRole("link", { name: /shop.*prod/ })).toHaveLength(2);
    expect(within(table).getByText("Failed")).toBeInTheDocument();
    expect(within(table).getByText("Running")).toBeInTheDocument();
    expect(within(table).getByText("1/3")).toBeInTheDocument();
    expect(screen.getByRole("note")).toHaveTextContent(/Authentication is disabled/);
  });

  it("parla italiano", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity({ auth_enabled: false }) };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/deployments") return { json: { deployments: [] } };
    });
    renderApp(api, "/deployments", "it");
    expect(await screen.findByText("Nessun deploy corrisponde a questi filtri.")).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Principale" })).toBeInTheDocument();
    expect(document.documentElement.lang).toBe("it");
  });
});

describe("approvazioni", () => {
  const waiting = dep({ id: "w1", state: "AWAITING_APPROVAL", environment: "prod", approval: { status: "pending", required: 1, expires_at: "2026-10-12T10:00:00Z" } });

  it("approva con un commento", async () => {
    const user = userEvent.setup();
    const { api, calls } = fakeApi(({ method, path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [waiting] } };
      if (method === "POST" && path === "/api/v1/deployments/w1/approve") return { status: 204 };
    });
    renderApp(api, "/approvals");
    await user.click(await screen.findByRole("button", { name: "Approve" }));
    const dialog = await screen.findByRole("dialog", { name: /Approve shop 1.4.0 in prod/ });
    await user.type(within(dialog).getByLabelText("Comment"), "CAB-42 ok");
    await user.click(within(dialog).getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(calls.some((c) => c.path === "/api/v1/deployments/w1/approve")).toBe(true));
    expect(calls.find((c) => c.path === "/api/v1/deployments/w1/approve")?.body).toEqual({ comment: "CAB-42 ok" });
    expect(await screen.findByText("Approval recorded.")).toBeInTheDocument();
  });

  it("per respingere serve un commento", async () => {
    const user = userEvent.setup();
    const { api, calls } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [waiting] } };
    });
    renderApp(api, "/approvals");
    await user.click(await screen.findByRole("button", { name: "Reject" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Reject" }));
    expect(within(dialog).getByText("A comment is required to reject.")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Comment")).toHaveAttribute("aria-invalid", "true");
    expect(calls.some((c) => c.path.endsWith("/reject"))).toBe(false);
  });

  it("non lascia approvare il proprio deploy", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity({ subject: "oidc:alice" }) };
      if (path === "/api/v1/approvals") return { json: { approvals: [waiting] } };
    });
    renderApp(api, "/approvals");
    const approve = await screen.findByRole("button", { name: "Approve" });
    expect(approve).toBeDisabled();
    expect(approve).toHaveAccessibleDescription("You requested this deployment, so you cannot approve it.");
  });
});

describe("inventario", () => {
  const inventory = [
    { application: "shop", environment: "test", current: dep({ id: "t1", environment: "test", version: "1.5.0" }) },
    { application: "shop", environment: "prod", current: dep({ id: "p1" }), drift: true },
  ];

  it("mostra la matrice, il drift e solo le azioni permesse", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/inventory") return { json: { inventory } };
    });
    renderApp(api, "/inventory");
    const table = await screen.findByRole("table", { name: /Live release per application/ });
    expect(within(table).getByRole("columnheader", { name: "test" })).toBeInTheDocument();
    expect(within(table).getByRole("link", { name: "1.5.0" })).toHaveAttribute("href", "/deployments/t1");
    expect(within(table).getByText(/Drift/)).toBeInTheDocument();
    // The approver can neither promote nor roll back.
    expect(within(table).queryByRole("button", { name: "Promote" })).toBeNull();
    expect(within(table).queryByRole("button", { name: "Roll back" })).toBeNull();
  });

  it("mostra i target cambiati fuori da manent", async () => {
    const checked = new Date().toISOString();
    const withTargets = [
      {
        application: "shop",
        environment: "test",
        current: dep({ id: "t1", environment: "test", version: "1.5.0" }),
        targets: { checked_at: checked, deployment: "t1", version: "1.5.0", targets: [{ host: "a", state: "in-sync", version: "1.5.0" }, { host: "b", state: "in-sync", version: "1.5.0" }] },
      },
      {
        application: "shop",
        environment: "prod",
        current: dep({ id: "p1" }),
        out_of_sync: true,
        targets: {
          checked_at: checked,
          deployment: "p1",
          version: "2.0.0",
          targets: [
            { host: "web-1", state: "in-sync", version: "2.0.0" },
            { host: "web-2", state: "drifted", version: "1.9.9" },
            { host: "web-3", state: "missing" },
          ],
        },
      },
    ];
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/inventory") return { json: { inventory: withTargets } };
    });
    renderApp(api, "/inventory");
    const table = await screen.findByRole("table", { name: /Live release per application/ });
    expect(within(table).getByText(/2 targets in sync/)).toBeInTheDocument();
    expect(within(table).getByText(/2 of 3 targets changed outside manent/)).toBeInTheDocument();
    expect(within(table).getByText("web-2: 1.9.9")).toBeInTheDocument();
    expect(within(table).getByText("web-3: no release")).toBeInTheDocument();
  });

  it("promuove solo dopo un dry-run riuscito", async () => {
    const user = userEvent.setup();
    const ops = identity({ grants: [{ role: "operator", actions: ["deployment.read", "deployment.submit", "deployment.dry-run", "deployment.rollback"], applications: ["*"], environments: ["*"] }] });
    const { api, calls } = fakeApi(({ method, path, body }) => {
      if (path === "/api/v1/me") return { json: ops };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/inventory") return { json: { inventory } };
      if (method === "POST" && path === "/api/v1/promotions") {
        const dry = (body as { dry_run?: boolean }).dry_run;
        return { status: 202, json: { id: dry ? "promo.dry-run" : "promo", state: "ENQUEUED" } };
      }
      if (path === "/api/v1/deployments/promo.dry-run") return { json: dep({ id: "promo.dry-run", dry_run: true }) };
    });
    renderApp(api, "/inventory");
    const group = await screen.findByRole("group", { name: "Actions for shop in test" });
    await user.click(within(group).getByRole("button", { name: "Promote" }));
    const dialog = await screen.findByRole("dialog", { name: "Promote shop from test" });
    const confirm = within(dialog).getByRole("button", { name: "Queue promote" });
    expect(confirm).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Run a dry-run" }));
    expect(await within(dialog).findByText("Dry-run succeeded: the deployment can run.")).toBeInTheDocument();
    await user.click(confirm);
    await waitFor(() => expect(calls.filter((c) => c.path === "/api/v1/promotions")).toHaveLength(2));
    expect(calls.filter((c) => c.path === "/api/v1/promotions").map((c) => c.body)).toEqual([
      { application: "shop", from: "test", to: "prod", dry_run: true },
      { application: "shop", from: "test", to: "prod" },
    ]);
  });
});

describe("attività", () => {
  const activity = {
    running: [dep({ id: "r1", state: "PENDING", executor: "worker-eu-1", last_step: "deploy", started_at: "2026-10-11T09:58:00Z" })],
    awaiting_approval: [],
    waiting_dependency: [],
    queued: [dep({ id: "q1", state: "ENQUEUED", environment: "test", queue: "deployments" })],
    delayed: [],
  };
  const workers = {
    workers: [
      {
        id: "worker-eu-1",
        state: "alive",
        region: "eu",
        version: "1",
        heartbeat_at: "2026-10-11T09:59:58Z",
        tasks: [{ id: "r1", deployment_id: "r1", kind: "deploy", application: "shop", environment: "prod" }],
        hidden_tasks: 2,
      },
      { id: "worker-us-1", state: "stale", tasks: [] },
    ],
  };

  it("mostra deploy attivi e worker a chi ha platform.read", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me")
        return { json: identity({ grants: [{ role: "operator", actions: ["deployment.read", "platform.read"], applications: ["*"], environments: ["prod"] }] }) };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/activity") return { json: activity };
      if (path === "/api/v1/workers") return { json: workers };
    });
    renderApp(api, "/activity");
    const running = await screen.findByRole("table", { name: "Running" });
    expect(within(running).getByRole("link", { name: /shop.*prod/ })).toHaveAttribute("href", "/deployments/r1");
    expect(within(running).getByText("step deploy")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Queued" })).toHaveTextContent("deployments");
    expect(screen.queryByRole("table", { name: "Scheduled" })).not.toBeInTheDocument();
    const ws = await screen.findByRole("table", { name: "Workers" });
    expect(within(ws).getByText("Alive")).toBeInTheDocument();
    expect(within(ws).getByText("Lost")).toBeInTheDocument();
    expect(within(ws).getByText("+2 not visible to you")).toBeInTheDocument();
    expect(within(ws).getByText("Idle")).toBeInTheDocument();
  });

  it("senza platform.read non chiede i worker", async () => {
    const { api, calls } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/activity") return { json: { running: [], awaiting_approval: [], waiting_dependency: [], queued: [], delayed: [] } };
    });
    renderApp(api, "/activity");
    expect(await screen.findByText("No active deployments.")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /Workers/ })).not.toBeInTheDocument();
    expect(calls.some((c) => c.path === "/api/v1/workers")).toBe(false);
  });
});

describe("catene", () => {
  it("mostra le dipendenze di un deploy in attesa", async () => {
    const waiting = dep({
      id: "api-2",
      state: "WAITING_DEPENDENCY",
      release: "r1",
      after: { status: "waiting", expires_at: "2026-10-12T10:00:00Z", dependencies: [{ id: "db-2", state: "PENDING" }] },
    });
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity({ auth_enabled: false }) };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/deployments/api-2") return { json: waiting };
      if (path === "/api/v1/deployments/api-2/steps") return { json: { steps: [] } };
      if (path === "/api/v1/deployments/api-2/logs") return { json: { lines: [], next: 0, done: false } };
      if (path === "/api/v1/deployments/api-2/events") return { status: 404 };
    });
    renderApp(api, "/deployments/api-2");
    const list = await screen.findByRole("list", { name: "Deploys this deploy depends on" });
    expect(within(list).getByRole("link", { name: "db-2" })).toHaveAttribute("href", "/deployments/db-2");
    expect(within(list).getByText("Running")).toBeInTheDocument();
    expect(screen.getAllByText("Waiting for dependencies").length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: "r1" })).toHaveAttribute("href", "/releases/r1");
  });

  it("mostra una release con i deploy e il rollback", async () => {
    const rel = {
      id: "r2",
      state: "ERROR",
      on_failure: "rollback",
      requested_by: "oidc:alice",
      created_at: now,
      updated_at: now,
      error: "failed deploys: api: ERROR",
      deploys: [
        { name: "db", deployment_id: "r2.db", application: "db", environment: "prod", version: "2.0.0", state: "SUCCESS", rollback_id: "r2.rollback.db", rollback_state: "SUCCESS" },
        { name: "api", deployment_id: "r2.api", application: "api", environment: "prod", version: "2.0.0", after: ["db"], state: "ERROR" },
        { name: "web", deployment_id: "r2.web", application: "web", environment: "prod", after: ["api"], state: "CANCELLED" },
      ],
    };
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity({ auth_enabled: false }) };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/releases") return { json: { releases: [rel] } };
      if (path === "/api/v1/releases/r2") return { json: rel };
    });
    const user = userEvent.setup();
    renderApp(api, "/releases");
    const table = await screen.findByRole("table", { name: "Releases, newest first" });
    expect(within(table).getByText("1/3 succeeded")).toBeInTheDocument();
    await user.click(within(table).getByRole("link", { name: "r2" }));
    const deploys = await screen.findByRole("table", { name: /Deploys of the release/ });
    const rows = within(deploys).getAllByRole("row");
    expect(rows[1]).toHaveTextContent(/db.*Succeeded.*Rollback.*Succeeded/);
    expect(rows[2]).toHaveTextContent(/api.*db.*Failed/);
    expect(screen.getByRole("alert")).toHaveTextContent("api: ERROR");
    expect(screen.queryByRole("button", { name: "Cancel release" })).not.toBeInTheDocument();
  });
});

describe("governance", () => {
  const releaseManager = identity({
    grants: [{ role: "release-manager", actions: ["deployment.read", "schedule.read", "freeze.write"], applications: ["*"], environments: ["prod"] }],
  });

  it("crea un freeze d'emergenza dal calendario", async () => {
    const user = userEvent.setup();
    const { api, calls } = fakeApi(({ method, path }) => {
      if (path === "/api/v1/me") return { json: releaseManager };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/schedules") return { json: { schedules: [] } };
      if (method === "GET" && path === "/api/v1/freezes")
        return {
          json: {
            freezes: [
              { name: "year-end", applications: ["*"], environments: ["prod"], active: false, start: now, end: now },
              { name: "incident-7", applications: ["*"], environments: ["prod"], active: true, hard: true, dynamic: true, created_by: "oidc:carol", start: now, end: now },
            ],
          },
        };
      if (method === "POST" && path === "/api/v1/freezes") return { status: 201, json: { name: "incident-8", applications: ["*"], environments: ["prod"], active: true } };
    });
    renderApp(api, "/calendar");
    const table = await screen.findByRole("table", { name: "Active and upcoming freeze windows" });
    // Only the windows created at runtime can be closed.
    expect(within(table).getAllByRole("button", { name: /Close/ })).toHaveLength(1);
    expect(within(table).getByText("Hard")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "New freeze" }));
    const dialog = await screen.findByRole("dialog", { name: "New freeze" });
    await user.type(within(dialog).getByLabelText("Name"), "incident-8");
    await user.type(within(dialog).getByLabelText("Reason"), "database outage");
    await user.click(within(dialog).getByLabelText(/Hard freeze/));
    await user.click(within(dialog).getByRole("button", { name: "Create freeze" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/freezes")).toBe(true));
    const body = calls.find((c) => c.method === "POST" && c.path === "/api/v1/freezes")?.body as Record<string, unknown>;
    expect(body).toMatchObject({ name: "incident-8", reason: "database outage", applications: ["*"], environments: ["prod"], hard: true });
    expect(await screen.findByText("Freeze incident-8 created.")).toBeInTheDocument();
  });

  it("senza freeze.write non mostra il nuovo freeze", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/schedules") return { json: { schedules: [] } };
      if (path === "/api/v1/freezes") return { json: { freezes: [] } };
    });
    renderApp(api, "/calendar");
    expect(await screen.findByText("No active or upcoming freeze windows.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New freeze" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Policy" })).toBeNull();
  });

  it("approva una revisione della policy", async () => {
    const user = userEvent.setup();
    const admin = identity({ grants: [{ role: "admin", actions: ["*"], applications: ["*"], environments: ["*"] }] });
    const { api, calls } = fakeApi(({ method, path }) => {
      if (path === "/api/v1/me") return { json: admin };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/policies")
        return {
          json: {
            engine: "opa",
            source: "store",
            current: "1:abc",
            revisions: [
              { revision: 2, sha256: "def0123456789", status: "draft", author: "oidc:alice", comment: "DBA approval", created_at: now, tests: [{ name: "t1", pass: true }] },
              { revision: 1, sha256: "abc0123456789", status: "active", author: "oidc:alice", reviewed_by: "oidc:bob", created_at: now },
            ],
          },
        };
      if (path === "/api/v1/policies/2")
        return { json: { revision: 2, sha256: "def0123456789", status: "draft", author: "oidc:alice", comment: "DBA approval", created_at: now, files: { "governance.rego": "package manent.governance" } } };
      if (path === "/api/v1/policies/2/impact")
        return { json: { revision: "2:def", since: now, evaluated: 4, changes: [{ deployment: "d9", application: "db", environment: "prod", version: "3.0", created_at: now, before: "approvals=1", after: "approvals=2" }] } };
      if (method === "POST" && path === "/api/v1/policies/2/approve") return { status: 204 };
    });
    renderApp(api, "/policies");
    const table = await screen.findByRole("table", { name: "Revisions" });
    expect(within(table).getByText("1 of 1 passed")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Policy" })).toBeInTheDocument();
    await user.click(within(table).getByRole("button", { name: "2" }));
    const dialog = await screen.findByRole("dialog", { name: "Revision 2" });
    expect(await within(dialog).findByText("package manent.governance")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Compute the impact" }));
    expect(await within(dialog).findByText("4 deploys evaluated, 1 with a different decision.")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Approve and activate" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/policies/2/approve")).toBe(true));
  });
});

describe("tenant", () => {
  it("mostra la matrice tenant × ambiente", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/inventory")
        return {
          json: {
            inventory: [
              { application: "shop", environment: "prod", tenant: "acme", current: dep({ id: "a1", version: "1.4.0", tenant: "acme" }) },
              { application: "shop", environment: "prod", tenant: "globex", current: dep({ id: "g1", version: "1.3.0", tenant: "globex" }) },
            ],
          },
        };
    });
    renderApp(api, "/inventory");
    const table = await screen.findByRole("table", { name: /Live release per application/ });
    expect(within(table).getByRole("columnheader", { name: "Tenant" })).toBeInTheDocument();
    expect(within(table).getByText("acme")).toBeInTheDocument();
    expect(within(table).getByRole("link", { name: "1.3.0" })).toHaveAttribute("href", "/deployments/g1");
  });

  it("mostra lo stato di un rilascio multi-tenant", async () => {
    const run = dep({
      id: "shop-100",
      state: "PENDING",
      kind: "tenants",
      tenants_run: {
        id: "shop-100", application: "shop", environment: "prod", status: "running", group: "pilota", max_parallel: 2, on_failure: "stop",
        groups: [{ name: "pilota", state: "running" }, { name: "resto", state: "pending" }],
        tenants: [
          { tenant: "acme", group: "pilota", deployment_id: "shop-100.acme", state: "SUCCESS" },
          { tenant: "initech", group: "resto", state: "DENIED", reason: "contract 2025" },
        ],
      },
    });
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/deployments/shop-100") return { json: run };
      if (path === "/api/v1/deployments/shop-100/steps") return { json: { steps: [] } };
      if (path === "/api/v1/deployments/shop-100/logs") return { json: { lines: [], next_offset: 0, done: false } };
    });
    renderApp(api, "/deployments/shop-100");
    const table = await screen.findByRole("table", { name: "Deploys of the tenants of the release" });
    expect(within(table).getByRole("link", { name: "acme" })).toHaveAttribute("href", "/deployments/shop-100.acme");
    expect(within(table).getByText("Denied by the policy")).toBeInTheDocument();
    expect(within(table).getByText("contract 2025")).toBeInTheDocument();
  });
});

describe("piattaforma", () => {
  const ops = identity({ grants: [{ role: "operator", actions: ["deployment.read", "platform.read"], applications: ["*"], environments: ["*"] }] });
  const queues = {
    queues: [
      { name: "deployments", kind: "deployments", worker_concurrency: 4, partition_concurrency: 1, enqueued: 3, running: 2, delayed: 1, oldest_enqueued: "2026-10-11T09:58:00Z", workers: ["w1"] },
      { name: "deployments.us", kind: "region", region: "us", partition_concurrency: 1, enqueued: 1, running: 0, delayed: 0, workers: [] },
    ],
  };
  const workers = {
    workers: [
      {
        id: "w1",
        state: "alive",
        heartbeat_at: "2026-10-11T09:59:58Z",
        queues: ["deployments", "deployments-approvals"],
        resources: {
          os: "linux",
          arch: "amd64",
          cpus: 8,
          cpu_limit: 2,
          process_cpu_cores: 0.5,
          memory_total_bytes: 16 * 2 ** 30,
          memory_limit_bytes: 2 ** 30,
          process_memory_bytes: 512 * 2 ** 20,
          goroutines: 40,
          concurrency: 4,
          workdir_free_bytes: 20 * 2 ** 30,
          load1: 0.4,
          load5: 0.3,
          load15: 0.2,
          sampled_at: "2026-10-11T09:59:58Z",
        },
        tasks: [{ id: "r1", deployment_id: "r1", kind: "deploy", application: "shop", environment: "prod", queue: "deployments" }],
      },
      { id: "w2", state: "alive", tasks: [] },
    ],
  };

  it("mostra code, limiti, risorse e code ascoltate dai worker", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: ops };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/queues") return { json: queues };
      if (path === "/api/v1/workers") return { json: workers };
    });
    renderApp(api, "/platform");
    expect(await screen.findByRole("link", { name: "Platform" })).toHaveAttribute("href", "/platform");
    const table = await screen.findByRole("table", { name: "Queues" });
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("deployments");
    expect(rows[1]).toHaveTextContent("4 per worker, 1 per environment");
    expect(rows[2]).toHaveTextContent("Region passes · us");
    expect(rows[2]).toHaveTextContent("No worker");
    expect(await screen.findByRole("meter", { name: "CPU" })).toHaveAttribute("aria-valuetext", "0.5 of 2 cores");
    expect(screen.getByRole("meter", { name: "Memory" })).toHaveAttribute("aria-valuetext", "512 MiB / 1 GiB");
    expect(screen.getByRole("meter", { name: "Deploy slots" })).toHaveAttribute("aria-valuetext", "1 of 4 busy");
    expect(screen.getByText("deployments-approvals")).toBeInTheDocument();
    expect(screen.getByText("This worker has not reported its resources (older version).")).toBeInTheDocument();
  });

  it("senza platform.read il menu non mostra la piattaforma", async () => {
    const { api } = fakeApi(({ path }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/deployments") return { json: { deployments: [] } };
    });
    renderApp(api, "/deployments");
    expect(await screen.findByRole("link", { name: "Deployments" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Platform" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Tenants" })).not.toBeInTheDocument();
  });
});

describe("tenant", () => {
  const tenants = {
    tenants: [
      { name: "acme", description: "ACME Corp", tags: { tier: "gold" }, applications: { shop: ["prod", "staging"] }, params: { shop: { db: "acme_db" } }, secrets: { shop: ["db-password"] }, source: "file:envconfig/tenants/acme.yml" },
      { name: "initech", tags: { tier: "silver" }, applications: { shop: ["prod"] }, disabled: true },
    ],
  };

  it("elenca i tenant con tag e ambienti e filtra per selettore", async () => {
    const { api, calls } = fakeApi(({ path, query }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/tenants") return { json: query.get("selector") === "tier=gold" ? { tenants: [tenants.tenants[0]] } : tenants };
    });
    renderApp(api, "/tenants");
    expect(await screen.findByRole("link", { name: "Tenants" })).toHaveAttribute("href", "/tenants");
    const table = await screen.findByRole("table", { name: "Tenants" });
    expect(within(table).getByRole("link", { name: "acme" })).toHaveAttribute("href", "/tenants/acme");
    expect(within(table).getByText("tier=gold")).toBeInTheDocument();
    expect(within(table).getByText("Disabled")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/Selector/), "tier=gold");
    await userEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(within(screen.getByRole("table", { name: "Tenants" })).queryByText("initech")).not.toBeInTheDocument());
    expect(calls.some((c) => c.path === "/api/v1/tenants")).toBe(true);
  });

  it("mostra il dettaglio con le release in esercizio, i parametri e solo i nomi dei segreti", async () => {
    const { api } = fakeApi(({ path, query }) => {
      if (path === "/api/v1/me") return { json: identity() };
      if (path === "/api/v1/approvals") return { json: { approvals: [] } };
      if (path === "/api/v1/tenants") return { json: tenants };
      if (path === "/api/v1/tenants/acme") return { json: tenants.tenants[0] };
      if (path === "/api/v1/inventory" && query.get("tenant") === "acme")
        return { json: { inventory: [{ application: "shop", environment: "prod", tenant: "acme", current: dep({ id: "shop-1.4.0-prod.acme", tenant: "acme" }) }] } };
    });
    renderApp(api, "/tenants/acme");
    expect(await screen.findByRole("heading", { name: "acme" })).toBeInTheDocument();
    const releases = await screen.findByRole("table", { name: "Releases in service" });
    const rows = within(releases).getAllByRole("row");
    await waitFor(() => expect(rows[1]).toHaveTextContent("1.4.0"));
    expect(rows[2]).toHaveTextContent("never released");
    expect(screen.getByText("acme_db")).toBeInTheDocument();
    expect(screen.getByText("db-password")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Deployment history →" })).toHaveAttribute("href", "/deployments?application=shop&tenant=acme");
  });
});
