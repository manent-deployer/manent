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
