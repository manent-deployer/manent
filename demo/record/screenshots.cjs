// Takes the screenshots of the web interface for the README and the documentation, in English
// (<name>.png) and Italian (<name>.it.png), dark theme. Run by demo/record/screenshots.sh after
// the deploys it needs. The demo identity provider signs in John Doe with no login page.
//
//   node demo/record/screenshots.cjs <url> <out dir>
//
// Uses Playwright from web/node_modules and the installed Google Chrome (no browser download).
const path = require("path");
const { createRequire } = require("module");
const req = createRequire(path.join(__dirname, "../../web/package.json"));
const { chromium } = req("@playwright/test");

const [url = "http://localhost:8080", out = "docs/content/assets/screenshots"] = process.argv.slice(2);

// name, path, what the page shows once its data is loaded.
const shots = [
  ["deployments", "/deployments", (page) => page.getByRole("table").waitFor()],
  ["deployment-waves", "/deployments/shop-1.3.0", (page) => page.getByRole("region", { name: /Waves|Ondate/ }).waitFor()],
  ["deployment-rollback", "/deployments/shop-1.4.0", (page) => page.getByRole("region", { name: /Waves|Ondate/ }).waitFor()],
  ["inventory", "/inventory", (page) => page.getByRole("table").waitFor()],
  // Awaiting approval, scheduled, workers.
  ["activity", "/activity", (page) => page.getByRole("table").nth(2).waitFor()],
  ["metrics", "/dora", (page) => page.getByRole("region").locator("svg").first().waitFor()],
];

(async () => {
  const browser = await chromium.launch({ channel: "chrome" });
  for (const [locale, suffix] of [["en-US", ""], ["it-IT", ".it"]]) {
    // 1280x900 at 1.25: 1600x1125 images, like the previous ones.
    const context = await browser.newContext({
      viewport: { width: 1280, height: 900 },
      deviceScaleFactor: 1.25,
      colorScheme: "dark",
      locale,
    });
    const page = await context.newPage();
    for (const [name, route, ready] of shots) {
      await page.goto(url + route);
      await ready(page);
      await page.waitForLoadState("networkidle");
      // Entry animations and charts.
      await page.waitForTimeout(1200);
      const file = path.join(out, `${name}${suffix}.png`);
      await page.screenshot({ path: file });
      console.log(file);
    }
    await context.close();
  }
  await browser.close();
})();
