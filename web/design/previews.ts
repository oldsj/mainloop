import { mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import type { Plugin } from 'vite';

const root = join(import.meta.dirname, '..');
export const previewSource = join(root, 'design/.preview-build');

function discover(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true })
    .flatMap((entry) => {
      const path = join(dir, entry.name);
      return entry.isDirectory() ? discover(path) : path.endsWith('.preview.tsx') ? [path] : [];
    })
    .sort();
}

export function designPreviews(): Plugin {
  return {
    name: 'design-previews',
    config() {
      mkdirSync(previewSource, { recursive: true });
      const files = discover(join(root, 'src'));
      if (!files.length) throw new Error('No component previews found');
      const inputs: Record<string, string> = {};
      const links: string[] = [];
      const html = (title: string, body: string) => `<!doctype html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"><title>${title} — Mainloop design system</title></head><body class="bg-background text-foreground font-mono"><main class="p-lg"><h1 class="text-xl mb-lg">${title}</h1>${body}</main></body></html>`;
      for (const file of files) {
        const name = relative(join(root, 'src'), file)
          .replace(/\.preview\.tsx$/, '')
          .replaceAll('/', '-');
        const title = file.split('/').at(-1)!.replace('.preview.tsx', '');
        const entry = `${name}.tsx`;
        writeFileSync(
          join(previewSource, entry),
          `import { createRoot } from 'react-dom/client';
import * as previews from ${JSON.stringify(file)};
import '../../src/styles/index.css';
const variants = Object.entries(previews);
const requested = new URLSearchParams(location.search).get('variant') ?? 'default';
const Preview = previews[requested as keyof typeof previews];
document.getElementById('preview')!.dataset.variants = JSON.stringify(variants.map(([name]) => name));
function App() { return <section aria-label={requested}><Preview /></section>; }
createRoot(document.getElementById('preview')!).render(<App />);
`
        );
        const path = join(previewSource, `${name}.html`);
        writeFileSync(
          path,
          html(title, `<div id="preview"></div><script type="module" src="./${entry}"></script>`)
        );
        inputs[name] = path;
        links.push(`<li><a href="./${name}.html">${title}</a></li>`);
        for (const match of readFileSync(file, 'utf8').matchAll(
          /export function (\w+Preview)\(/g
        )) {
          links.push(
            `<li><a href="./${name}.html?variant=${match[1]}">${title}: ${match[1]}</a></li>`
          );
        }
      }
      const index = join(previewSource, 'index.html');
      writeFileSync(
        index,
        html(
          'Component previews',
          `<ul>${links.join('\n')}</ul><script type="module" src="./index.ts"></script>`
        )
      );
      writeFileSync(join(previewSource, 'index.ts'), "import '../../src/styles/index.css';\n");
      inputs.index = index;
      return {
        root: previewSource,
        base: './',
        build: {
          outDir: join(root, 'design/system/previews'),
          emptyOutDir: true,
          rollupOptions: { input: inputs }
        }
      };
    }
  };
}
