// Records the web interface part of the demo GIF as a video, after the terminal part: the
// deployments list, the failed release with its waves, rollback and logs, the inventory. The
// demo identity provider signs in John Doe with no login page.
//
//   node demo/record/browser.cjs <url> <out dir>
//
// Uses Playwright from web/node_modules and the installed Google Chrome (no browser download).
const path = require("path");
const { createRequire } = require("module");
const req = createRequire(path.join(__dirname, "../../web/package.json"));
const { chromium } = req("@playwright/test");

const [url = "http://localhost:8080", out = "."] = process.argv.slice(2);
const size = { width: 1280, height: 800 };

(async () => {
  const browser = await chromium.launch({ channel: "chrome" });
  const context = await browser.newContext({
    viewport: size,
    deviceScaleFactor: 1,
    colorScheme: "dark",
    locale: "en-US",
    recordVideo: { dir: out, size },
  });
  const page = await context.newPage();
  const pause = (ms) => page.waitForTimeout(ms);
  // Smooth scroll to a section of the page (Section: a region named by its heading).
  const show = async (name, block = "start") => {
    await page.getByRole("region", { name }).evaluate((el, block) => el.scrollIntoView({ behavior: "smooth", block }), block);
    await pause(900);
  };

  await page.goto(url + "/deployments");
  await page.getByRole("table").waitFor();
  await pause(2500);

  await page.getByRole("link", { name: /shop.*prod/ }).first().click();
  await page.getByRole("region", { name: /Waves/ }).waitFor();
  await pause(2000);
  // Centered: at the start the cards would end up under the sticky header.
  await show(/Waves/, "center");
  await pause(2500);
  await show(/Logs/);
  await pause(2500);

  await page.goto(url + "/inventory");
  await page.getByRole("table").waitFor();
  await pause(2500);

  await page.getByRole("link", { name: "Deployments of shop", exact: true }).click();
  await page.getByRole("table").waitFor();
  await pause(2500);

  const video = page.video();
  await context.close();
  console.log(await video.path());
  await browser.close();
})();
