import { QueryClient } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { AppWithConfig } from "@/App";
import type { Api } from "@/api/client";
import type { Lang } from "@/i18n/i18n";
import type { UIConfig } from "@/lib/config";

import { dep, fakeApi, identity } from "./fakeApi";

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
    // L'approvatore non può promuovere né fare rollback.
    expect(within(table).queryByRole("button", { name: "Promote" })).toBeNull();
    expect(within(table).queryByRole("button", { name: "Roll back" })).toBeNull();
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
