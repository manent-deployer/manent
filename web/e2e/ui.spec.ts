import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// Data prepared by uc8.sh: a finished uc8-web deploy in test and the governance rule that
// asks for an approval in prod.
const DEP_ID = process.env.DEP_ID ?? "";

async function login(page: Page, user: string, path = "/deployments") {
  await page.goto(path);
  // The test identity provider only asks which user to use.
  await page.getByRole("button", { name: user, exact: true }).click();
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
}

async function expectAccessible(page: Page) {
  // AxeBuilder depends on its own playwright-core version: at runtime the page is compatible.
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
  // Light theme: the contrast of the second palette is checked too.
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "Inventory" }).click();
  await expectAccessible(page);
  // The viewer sees no actions.
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
  // Promotion to prod requires an approval: whoever asked for it cannot give it.
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
  // Reloading the page loses the in-memory token: the identity provider session completes the
  // login again without asking for the user.
  await page.goto("/deployments?application=uc8-web&environment=prod");
  await page.getByRole("link", { name: /uc8-web.*prod/ }).first().click();
  await expect(page.getByText("Live")).toBeVisible();
  // The state arrives from the SSE stream, without reloading the page.
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
