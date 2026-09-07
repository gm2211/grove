// Captures the README/docs screenshots against the mock UI (VITE_MOCK=1).
//
// Usage: npm run screenshots
//   1. starts `vite` with VITE_MOCK=1 on an ephemeral port
//   2. drives it with Playwright (dark theme, 1440x900, deviceScaleFactor 1)
//   3. writes PNGs to ../docs/screenshots/
//   4. shuts the dev server down
//
// Kept intentionally small and dependency-light (no playwright test runner) so it's easy to
// re-run whenever the UI changes and the screenshots go stale.
import { chromium } from "playwright";
import { spawn } from "node:child_process";
import { mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const uiRoot = path.resolve(__dirname, "..");
const outDir = path.resolve(uiRoot, "../docs/screenshots");
const port = 4173;
const baseURL = `http://localhost:${port}`;

mkdirSync(outDir, { recursive: true });

function waitForServer(url, timeoutMs = 30_000) {
  const start = Date.now();
  return new Promise((resolve, reject) => {
    const tick = async () => {
      try {
        const res = await fetch(url);
        if (res.ok) return resolve();
      } catch {
        // not up yet
      }
      if (Date.now() - start > timeoutMs) return reject(new Error(`timed out waiting for ${url}`));
      setTimeout(tick, 300);
    };
    tick();
  });
}

async function main() {
  console.log("[screenshots] starting vite (mock mode)...");
  const server = spawn("npx", ["vite", "--port", String(port), "--strictPort"], {
    cwd: uiRoot,
    env: { ...process.env, VITE_MOCK: "1" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  server.stdout.on("data", (d) => process.stdout.write(`[vite] ${d}`));
  server.stderr.on("data", (d) => process.stderr.write(`[vite] ${d}`));

  const cleanup = () => {
    server.kill("SIGTERM");
  };
  process.on("exit", cleanup);
  process.on("SIGINT", () => {
    cleanup();
    process.exit(1);
  });

  try {
    await waitForServer(baseURL);
    console.log("[screenshots] vite is up, launching chromium...");

    const browser = await chromium.launch();
    const context = await browser.newContext({
      viewport: { width: 1440, height: 900 },
      deviceScaleFactor: 1,
      colorScheme: "dark",
    });
    const page = await context.newPage();

    // 1. Fleet page
    await page.goto(`${baseURL}/fleet`, { waitUntil: "networkidle" });
    await page.waitForSelector("text=grove");
    await page.waitForTimeout(500);
    await page.screenshot({ path: path.join(outDir, "fleet.png") });
    console.log("[screenshots] captured fleet.png");

    // 2. Jobs list
    await page.goto(`${baseURL}/jobs`, { waitUntil: "networkidle" });
    await page.waitForTimeout(500);
    await page.screenshot({ path: path.join(outDir, "jobs.png") });
    console.log("[screenshots] captured jobs.png");

    // 3. Dispatch form (filled in, before submitting)
    await page.goto(`${baseURL}/dispatch`, { waitUntil: "networkidle" });
    await page.waitForTimeout(300);
    await page.selectOption("select >> nth=0", "build"); // kind
    const poolSelect = page.locator("select >> nth=1");
    const poolOptions = await poolSelect.locator("option").allTextContents();
    if (poolOptions.length > 0 && poolOptions[0] !== "no pools observed") {
      await poolSelect.selectOption(poolOptions[0]);
    }
    await page.fill('input[placeholder="github.com/gm2211/grove"]', "github.com/gm2211/grove");
    await page.fill('input[placeholder="main"]', "main");
    await page.fill("textarea", "make test && make build");
    await page.screenshot({ path: path.join(outDir, "dispatch.png") });
    console.log("[screenshots] captured dispatch.png");

    // 4. Job detail with a live log: submit the filled form, then capture while it's "running"
    await page.click('button[type="submit"]');
    await page.waitForURL(/\/jobs\/.+/);
    // the mock lifecycle flips pending -> running at ~2.5s and running -> terminal at ~9s;
    // wait until we're solidly in the "running" window with a few log lines streamed in.
    await page.waitForTimeout(4500);
    await page.screenshot({ path: path.join(outDir, "job-detail.png") });
    console.log("[screenshots] captured job-detail.png");

    // 5. Settings
    await page.goto(`${baseURL}/settings`, { waitUntil: "networkidle" });
    await page.waitForTimeout(300);
    await page.screenshot({ path: path.join(outDir, "settings.png") });
    console.log("[screenshots] captured settings.png");

    await browser.close();
    console.log(`[screenshots] done — wrote PNGs to ${outDir}`);
  } finally {
    cleanup();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
