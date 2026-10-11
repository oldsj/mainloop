import { readdirSync } from 'node:fs';
import { join } from 'node:path';
import { chromium } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { preview } from 'vite';

const root = join(import.meta.dirname, 'system/previews');
const pages = readdirSync(root)
  .filter((name) => name.endsWith('.html'))
  .sort();
if (pages.length < 2 || !pages.includes('index.html')) throw new Error('Build the previews first');
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
  const context = await browser.newContext();
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
    if (errors.length) throw new Error(`${name}: ${errors.join('; ')}`);
    console.log(`${name}: checked (${results.violations.length} violations)`);
    page.removeAllListeners('pageerror');
  }
  console.log(
    `${pages.length} pages and ${queue.length - pages.length} additional variants checked; ${failures} serious/critical violations`
  );
  if (failures) process.exitCode = 1;
} finally {
  await browser?.close();
  await new Promise<void>((resolve, reject) =>
    server.httpServer.close((error) => (error ? reject(error) : resolve()))
  );
}
