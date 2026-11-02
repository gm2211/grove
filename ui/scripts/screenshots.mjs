// Captures the README/docs screenshots and the walkthrough GIF against the mock UI (VITE_MOCK=1).
//
// Usage: npm run screenshots
//   1. starts `vite` with VITE_MOCK=1 on an ephemeral port
//   2. drives it with Playwright (1440x900): stills at 2x, the walkthrough recorded as video
//   3. writes PNGs and walkthrough.gif to ../docs/screenshots/ (the GIF needs ffmpeg on PATH)
//   4. shuts the dev server down
//
// The mock fleet is deterministic (src/mock/data.ts), so re-running gives the same pictures.
// Navigation inside a run is by clicking, never page.goto: the mock's state lives in the page,
// and a reload would bring back the Macs we just approved.
import { chromium } from "playwright";
import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import path from "node:path";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const uiRoot = path.resolve(__dirname, "..");
const outDir = path.resolve(uiRoot, "../docs/screenshots");
const port = 4173;
const baseURL = `http://localhost:${port}`;
const viewport = { width: 1440, height: 900 };

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

const pause = (page, ms) => page.waitForTimeout(ms);
const nav = (page, label) => page.locator("nav a", { hasText: label }).click();
const node = (page, name) => page.locator(".mapnode", { hasText: name }).first().click();

async function stills(browser) {
  const context = await browser.newContext({ viewport, deviceScaleFactor: 2, colorScheme: "dark" });
  const page = await context.newPage();
  const shot = async (name) => {
    await page.screenshot({ path: path.join(outDir, name) });
    console.log(`[screenshots] captured ${name}`);
  };

  await page.goto(`${baseURL}/map`, { waitUntil: "networkidle" });
  // The hero shot is the map at work, without the join banner pushing the columns down.
  for (let i = 0; i < 2; i++) {
    await page.getByRole("button", { name: "Approve" }).first().click();
    await pause(page, 700);
  }
  await pause(page, 4500); // let the selected job's log tail fill in
  await shot("map.png");

  await node(page, "macos-mac-studio-1-0");
  await pause(page, 800);
  await shot("map-vm.png");

  await nav(page, "Jobs");
  await pause(page, 800);
  await shot("jobs.png");

  await page.locator(".trow", { hasText: "grove@pr-151" }).click();
  await pause(page, 4000);
  await shot("job-detail.png");

  await nav(page, "Dispatch");
  await pause(page, 500);
  await page.fill('input[name="repo"]', "https://github.com/gm2211/grove");
  await shot("dispatch.png");

  await nav(page, "Settings");
  await pause(page, 600);
  await shot("settings.png");

  await context.close();
}

async function walkthrough(browser) {
  const videoDir = mkdtempSync(path.join(tmpdir(), "grove-walkthrough-"));
  const context = await browser.newContext({
    viewport,
    deviceScaleFactor: 1,
    colorScheme: "dark",
    recordVideo: { dir: videoDir, size: viewport },
  });
  const page = await context.newPage();
  const started = Date.now();

  await page.goto(`${baseURL}/map`, { waitUntil: "networkidle" });
  const trimStart = (Date.now() - started) / 1000;
  await pause(page, 2200);
  // A Mac is waiting to join: approve it from the map.
  await page.getByRole("button", { name: "Approve" }).first().click();
  await pause(page, 1200);
  await page.getByRole("button", { name: "Approve" }).first().click();
  await pause(page, 2500);
  // Walk the tree: a snoozed Mac, then a full one, its macOS VM, and the queue behind it.
  await node(page, "mac-mini-2");
  await pause(page, 2200);
  await node(page, "mac-mini-1");
  await pause(page, 1200);
  await node(page, "macos-mac-mini-1-0");
  await pause(page, 2000);
  await page.locator(".mapcol").nth(0).locator(".mapnode").first().click();
  await pause(page, 2400);
  await node(page, "mac-studio-1");
  await pause(page, 1000);
  await node(page, "linux-mac-studio-1-0");
  await pause(page, 900);
  await node(page, "grove@pr-151");
  await pause(page, 3000);
  // The same fleet as a table.
  await page.getByRole("button", { name: "Table" }).click();
  await pause(page, 2200);
  await page.getByRole("button", { name: "Graph" }).click();
  await pause(page, 800);
  // Jobs, filtered, then one job's lifecycle and live log.
  await nav(page, "Jobs");
  await pause(page, 1600);
  await page.locator(".selbtn", { hasText: "running" }).click();
  await pause(page, 1600);
  await page.locator(".trow", { hasText: "grove@pr-151" }).click();
  await pause(page, 4000);
  await nav(page, "Dispatch");
  await pause(page, 1800);

  const video = page.video();
  await context.close();
  const webm = await video.path();

  const gif = path.join(outDir, "walkthrough.gif");
  const ff = spawnSync(
    "ffmpeg",
    [
      "-y",
      "-loglevel", "error",
      "-ss", String(trimStart + 0.3),
      "-i", webm,
      "-vf",
      "fps=8,scale=1000:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=48:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle",
      "-loop", "0",
      gif,
    ],
    { stdio: "inherit" },
  );
  if (ff.error || ff.status !== 0) {
    console.warn("[screenshots] ffmpeg failed or is missing; walkthrough.gif not written");
  } else {
    console.log("[screenshots] captured walkthrough.gif");
  }
  for (const f of readdirSync(videoDir)) rmSync(path.join(videoDir, f));
  rmSync(videoDir, { recursive: true, force: true });
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

  const cleanup = () => server.kill("SIGTERM");
  process.on("exit", cleanup);
  process.on("SIGINT", () => {
    cleanup();
    process.exit(1);
  });

  try {
    await waitForServer(baseURL);
    console.log("[screenshots] vite is up, launching chromium...");
    const browser = await chromium.launch();
    await stills(browser);
    if (!process.argv.includes("--no-gif")) await walkthrough(browser);
    await browser.close();
    console.log(`[screenshots] done, wrote to ${outDir}`);
  } finally {
    cleanup();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
