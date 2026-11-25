import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { chromium } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { preview } from 'vite';

const root = join(import.meta.dirname, 'system/previews');
const pages = readdirSync(root)
  .filter((name) => name.endsWith('.html'))
  .sort();
if (pages.length < 2 || !pages.includes('index.html')) throw new Error('Build the previews first');
// The previews must carry the components' own classes, or every page here is checked unstyled (and
// its contrast with it). `min-h-control` is a class only components use, never the preview pages.
const css = readdirSync(join(root, 'assets'))
  .filter((name) => name.endsWith('.css'))
  .map((name) => readFileSync(join(root, 'assets', name), 'utf8'))
  .join('');
if (!css.includes('.min-h-control'))
  throw new Error("The previews were built without the components' Tailwind classes");
const DESKTOP = { width: 1280, height: 720 };
const PHONE = { width: 390, height: 844 };
const server = await preview({
  configFile: false,
  root: import.meta.dirname,
  build: { outDir: 'system/previews' },
  preview: { host: '127.0.0.1', port: 0 }
});
let browser;
let failures = 0;
try {
  browser = await chromium.launch();
  const context = await browser.newContext({ viewport: DESKTOP });
  const page = await context.newPage();
  const queue = [...pages];
  for (let i = 0; i < queue.length; i++) {
    const name = queue[i]!;
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.goto(`${server.resolvedUrls!.local[0]}${name}`);
    if (name !== 'index.html')
      await page.locator('#preview section').first().waitFor({ state: 'attached', timeout: 10000 });
    if (name !== 'index.html' && !name.includes('?')) {
      const variants = JSON.parse(
        (await page.locator('#preview').getAttribute('data-variants'))!
      ) as string[];
      queue.push(
        ...variants
          .filter((variant) => variant !== 'default')
          .map((variant) => name + '?variant=' + variant)
      );
    }
    await page.evaluate(() => document.fonts.ready);
    const results = await new AxeBuilder({ page }).analyze();
    for (const violation of results.violations) {
      console.log(
        `${name}: ${violation.impact} ${violation.id} ${JSON.stringify(violation.nodes.map((node) => ({ target: node.target, summary: node.failureSummary })))}`
      );
      if (violation.impact === 'serious' || violation.impact === 'critical') failures++;
    }
    // On a phone (390 px wide) nothing may run past the preview's own edge: the app is monospace,
    // which runs wider than a sans, so a row that fits on a desktop can push a control off-screen.
    // Content inside a scroll or clipping container, and fixed overlays, are exempt.
    await page.setViewportSize(PHONE);
    const spills = await page.evaluate(() => {
      const root = document.getElementById('preview');
      if (!root) return []; // the index page lists the previews; it has none of its own
      const edge = root.getBoundingClientRect();
      const contained = (el: Element): boolean => {
        for (let up = el.parentElement; up && up !== root; up = up.parentElement)
          if (getComputedStyle(up).overflowX !== 'visible') return true;
        return false;
      };
      return Array.from(root.querySelectorAll('*'))
        .filter((el) => {
          const box = el.getBoundingClientRect();
          if (box.width === 0 || getComputedStyle(el).position === 'fixed' || contained(el))
            return false;
          return box.right > edge.right + 0.5 || box.left < edge.left - 0.5;
        })
        .map(
          (el) =>
            `${el.tagName.toLowerCase()} "${(el.textContent ?? '').trim().slice(0, 40)}" (${Math.round(el.getBoundingClientRect().right - edge.right)} px past the edge)`
        )
        .slice(0, 5);
    });
    await page.setViewportSize(DESKTOP);
    for (const spill of spills) console.log(`${name}: serious overflow at 390 px: ${spill}`);
    failures += spills.length;
    if (errors.length) throw new Error(`${name}: ${errors.join('; ')}`);
    console.log(`${name}: checked (${results.violations.length} violations)`);
    page.removeAllListeners('pageerror');
  }
  console.log(
    `${pages.length} pages and ${queue.length - pages.length} additional variants checked; ${failures} serious/critical violations or overflows at 390 px`
  );
  if (failures) process.exitCode = 1;
} finally {
  await browser?.close();
  await new Promise<void>((resolve, reject) =>
    server.httpServer.close((error) => (error ? reject(error) : resolve()))
  );
}
