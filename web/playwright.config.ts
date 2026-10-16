import { defineConfig, devices } from "@playwright/test";

// End-to-end tests of the web interface against the use case lab (UC8): the UI served by
// manent-server, the API and the test identity provider. Run by test/usecases/cases/uc8.sh
// in the Playwright container on the lab network. The UI opens on localhost (forwarded to
// api:8080), because the PKCE login requires a secure context.
export default defineConfig({
  testDir: "./e2e",
  timeout: 90_000,
  expect: { timeout: 20_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"], ["json", { outputFile: "test-results/results.json" }]],
  use: {
    baseURL: process.env.MANENT_UI ?? "http://localhost:8080",
    locale: "en-GB",
    colorScheme: "dark",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: process.env.MANENT_UI
    ? undefined
    : { command: "node e2e/forward.mjs", url: "http://localhost:8080/healthz", reuseExistingServer: false, timeout: 30_000 },
});
