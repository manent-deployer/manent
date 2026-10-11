import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// Dati preparati da uc8.sh: un deploy concluso di uc8-web in test e la regola di governance
// che chiede un'approvazione in prod.
const DEP_ID = process.env.DEP_ID ?? "";

async function login(page: Page, user: string, path = "/deployments") {
  await page.goto(path);
  // L'identity provider di prova chiede solo quale utente usare.
  await page.getByRole("button", { name: user, exact: true }).click();
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
}

async function expectAccessible(page: Page) {
  // AxeBuilder dipende da una propria versione di playwright-core: a runtime la pagina è compatibile.
  const results = await new AxeBuilder({ page: page as never }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"]).analyze();
  const serious = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical");
  expect(serious.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`)).toEqual([]);
}

test.describe.configure({ mode: "serial" });

test("login OIDC, navigazione e accessibilità delle viste", async ({ page }) => {
  await login(page, "viewer");
  await expect(page.getByRole("heading", { level: 1, name: "Deployments" })).toBeVisible();
  await expect(page.getByRole("table", { name: "Deployments, newest first" })).toBeVisible();
  await expectAccessible(page);
  for (const [nav, title] of [
    ["Inventory", "Inventory"],
    ["Calendar", "Calendar"],
    ["Metrics", "Metrics"],
    ["Approvals", "Approvals"],
  ]) {
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: nav }).click();
    await expect(page.getByRole("heading", { level: 1, name: title })).toBeFocused();
    await expectAccessible(page);
  }
  // Tema chiaro: anche il contrasto della seconda palette è verificato.
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Inventory" }).click();
  await expectAccessible(page);
  // Il viewer non vede azioni.
  await expect(page.getByRole("button", { name: "Promote" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Roll back" })).toHaveCount(0);
});

test("dettaglio di un deploy con step e log", async ({ page }) => {
  test.skip(!DEP_ID, "DEP_ID non impostato");
  await login(page, "viewer", `/deployments/${DEP_ID}`);
  await expect(page.getByRole("heading", { level: 1, name: /uc8-web → test/ })).toBeVisible();
  await expect(page.getByText("Succeeded").first()).toBeVisible();
  await expect(page.getByRole("table", { name: "Steps recorded for this deployment" })).toContainText("deploy");
  await expect(page.getByRole("log", { name: "Deployment log" })).toContainText("deploy");
  await expectAccessible(page);
});

test("promozione con dry-run e divieto di auto-approvazione", async ({ page }) => {
  await login(page, "admin", "/inventory");
  const group = page.getByRole("group", { name: "Actions for uc8-web in test" });
  await group.getByRole("button", { name: "Promote" }).click();
  const dialog = page.getByRole("dialog", { name: "Promote uc8-web from test" });
  await dialog.getByLabel("Target environment").fill("prod");
  const confirm = dialog.getByRole("button", { name: "Queue promote" });
  await expect(confirm).toBeDisabled();
  await dialog.getByRole("button", { name: "Run a dry-run" }).click();
  await expect(dialog.getByText("Dry-run succeeded: the deployment can run.")).toBeVisible({ timeout: 60_000 });
  await confirm.click();
  // La promozione in prod richiede un'approvazione: chi l'ha chiesta non può darla.
  await expect(page.getByRole("heading", { level: 1, name: /uc8-web → prod/ })).toBeVisible();
  await expect(page.getByText("Awaiting approval").first()).toBeVisible();
  const approve = page.getByRole("button", { name: "Approve" });
  await expect(approve).toBeDisabled();
  await expect(approve).toHaveAccessibleDescription("You requested this deployment, so you cannot approve it.");
});

test("approvazione da un altro utente e avanzamento in tempo reale", async ({ page }) => {
  await login(page, "cab", "/approvals");
  const item = page.getByRole("listitem").filter({ hasText: "uc8-web" });
  await item.getByRole("button", { name: "Approve" }).click();
  const dialog = page.getByRole("dialog", { name: /Approve uc8-web .* in prod/ });
  await dialog.getByLabel("Comment").fill("CAB-UC8 ok");
  await dialog.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText("Approval recorded.")).toBeVisible();
  // Il ricaricamento della pagina perde il token in memoria: la sessione dell'identity provider
  // completa di nuovo il login senza chiedere l'utente.
  await page.goto("/deployments?application=uc8-web&environment=prod");
  await page.getByRole("link", { name: /uc8-web.*prod/ }).first().click();
  await expect(page.getByText("Live")).toBeVisible();
  // Lo stato arriva dallo stream SSE, senza ricaricare la pagina.
  await expect(page.getByText("Succeeded").first()).toBeVisible({ timeout: 90_000 });
  await expect(page.getByText("cab approved")).toBeVisible();
});

test("tastiera: link per saltare al contenuto e lingua italiana", async ({ page }) => {
  await login(page, "viewer");
  await page.keyboard.press("Tab");
  const skip = page.getByRole("link", { name: "Skip to content" });
  await expect(skip).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("main#main")).toBeFocused();
  await page.getByLabel("Language").selectOption("it");
  await expect(page.getByRole("heading", { level: 1, name: "Deploy" })).toBeVisible();
  await expect(page.locator("html")).toHaveAttribute("lang", "it");
});
