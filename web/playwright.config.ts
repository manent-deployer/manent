import { defineConfig, devices } from "@playwright/test";

// Test end-to-end dell'interfaccia web contro il laboratorio dei casi d'uso (UC8): la UI servita
// da manent-server, l'API e l'identity provider di prova. Eseguiti da test/usecases/cases/uc8.sh
// nel container di Playwright sulla rete del laboratorio. La UI si apre su localhost (inoltrato
// verso api:8080), perché il login con PKCE richiede un contesto sicuro.
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
