import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider
} from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Markdown } from './markdown';

// The frozen `frontend/src/lib/markdown.ts` had no test file, so these cases are written from its
// rules: raw HTML shows as text, only http(s) and mailto links are clickable (new tab, noopener),
// images show their alt text, and GFM tables and line breaks render.
function view(text: string) {
  return render(<Markdown>{text}</Markdown>).container;
}

describe('raw HTML', () => {
  it('is shown as text, never injected', () => {
    const container = view('<script>window.__pwned = true</script> after');
    expect(container.querySelector('script')).toBeNull();
    expect(container).toHaveTextContent('<script>window.__pwned = true</script> after');
    expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
  });

  it('keeps inline tags and angle brackets in prose visible', () => {
    const container = view('run `x` then open <file> and <b>bold</b>');
    expect(container.querySelector('b')).toBeNull();
    expect(container.querySelector('file')).toBeNull();
    expect(container).toHaveTextContent('open <file> and <b>bold</b>');
  });

  it('does not run event-handler attributes', () => {
    const container = view('<img src=x onerror="window.__pwned = true">');
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('[onerror]')).toBeNull();
  });

  it('shows an HTML block as text', () => {
    const container = view('<div onclick="x()">block</div>');
    expect(container.querySelector('div > div > div, [onclick]')).toBeNull();
    expect(container).toHaveTextContent('<div onclick="x()">block</div>');
  });
});

describe('links', () => {
  it('opens http and https links in a new tab with noopener noreferrer', () => {
    view('[docs](https://example.test/a?b=1) and [plain](http://example.test)');
    const secure = screen.getByRole('link', { name: 'docs' });
    expect(secure).toHaveAttribute('href', 'https://example.test/a?b=1');
    expect(secure).toHaveAttribute('target', '_blank');
    expect(secure).toHaveAttribute('rel', 'noopener noreferrer');
    // The URL parser's form, with the root path written out. It is the same link.
    expect(screen.getByRole('link', { name: 'plain' })).toHaveAttribute(
      'href',
      'http://example.test/'
    );
  });

  it('allows mailto links', () => {
    view('[mail](mailto:owner@example.test)');
    const link = screen.getByRole('link', { name: 'mail' });
    expect(link).toHaveAttribute('href', 'mailto:owner@example.test');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('keeps a link title', () => {
    view('[docs](https://example.test "the docs")');
    expect(screen.getByRole('link', { name: 'docs' })).toHaveAttribute('title', 'the docs');
  });

  it.each([
    ['javascript', '[go](javascript:alert(1))'],
    ['mixed-case javascript', '[go](JaVaScRiPt:alert(1))'],
    ['angle-bracketed javascript', '[go](<javascript:alert(1)>)'],
    ['data', '[go](data:text/html;base64,PHNjcmlwdD4=)'],
    ['vbscript', '[go](vbscript:msgbox(1))'],
    ['file', '[go](file:///etc/passwd)'],
    ['relative path', '[go](/sessions/1)'],
    ['protocol-relative', '[go](//evil.test/x)'],
    ['no scheme', '[go](example.test)'],
    ['reference definition', '[go][1]\n\n[1]: javascript:alert(1)']
  ])('renders a %s link as plain text', (_name, text) => {
    const container = view(text);
    expect(container.querySelector('a')).toBeNull();
    expect(container.querySelector('[href]')).toBeNull();
    expect(container).toHaveTextContent('go');
  });

  it('does not make a bare javascript: URL clickable', () => {
    const container = view('see javascript:alert(1) here');
    expect(container.querySelector('a')).toBeNull();
  });

  it('links bare http URLs (GFM autolink) safely', () => {
    view('visit https://example.test/x now');
    const link = screen.getByRole('link', { name: 'https://example.test/x' });
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('renders a task link as an in-app route, not a new-tab link', async () => {
    const root = createRootRoute({ component: Outlet });
    const router = createRouter({
      routeTree: root.addChildren([
        createRoute({
          getParentRoute: () => root,
          path: '/',
          component: () => <Markdown>{'[the task](/tasks/task-1)'}</Markdown>
        }),
        createRoute({
          getParentRoute: () => root,
          path: '/tasks/$id',
          component: () => <p>Task page</p>
        })
      ]),
      history: createMemoryHistory({ initialEntries: ['/'] })
    });
    render(<RouterProvider router={router} />);
    const link = await screen.findByRole('link', { name: 'the task' });
    expect(link).toHaveAttribute('href', '/tasks/task-1');
    expect(link).not.toHaveAttribute('target');
  });
});

describe('images', () => {
  it('render as their alt text, with no element', () => {
    const container = view('before ![a diagram](https://example.test/p.png) after');
    expect(container.querySelector('img')).toBeNull();
    expect(container).toHaveTextContent('before a diagram after');
  });

  it('never load a remote or data source', () => {
    const container = view('![x](data:image/svg+xml;base64,AAAA) ![y](javascript:alert(1))');
    expect(container.querySelector('img, [src]')).toBeNull();
  });

  it('show alt text escaped', () => {
    const container = view('![<b>alt</b>](https://example.test/p.png)');
    expect(container.querySelector('b')).toBeNull();
    expect(container).toHaveTextContent('<b>alt</b>');
  });
});

describe('formatting', () => {
  it('renders GFM tables', () => {
    view('| a | b |\n| - | - |\n| 1 | 2 |');
    expect(screen.getByRole('table')).toBeInTheDocument();
    expect(screen.getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual(['a', 'b']);
    expect(screen.getAllByRole('cell').map((cell) => cell.textContent)).toEqual(['1', '2']);
  });

  it('scrolls a wide table inside a wrapper, keeping real table semantics', () => {
    const container = view('| a | b |\n| - | - |\n| 1 | 2 |');
    const table = container.querySelector('table');
    expect(table?.parentElement?.className).toContain('overflow-x-auto');
    expect(table?.className).not.toContain('block');
  });

  it('renders a single newline as a line break', () => {
    const container = view('one\ntwo');
    expect(container.querySelectorAll('br')).toHaveLength(1);
    expect(container).toHaveTextContent('one two');
  });

  it('renders lists, code, emphasis and headings', () => {
    const container = view('# Title\n\n- a\n- b\n\n`inline`\n\n```\nblock\n```\n\n**bold** _it_');
    expect(screen.getByRole('heading', { name: 'Title' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(container.querySelector('pre code')).toHaveTextContent('block');
    expect(container.querySelector('strong')).toHaveTextContent('bold');
    expect(container.querySelector('em')).toHaveTextContent('it');
  });

  it('renders nothing for empty text', () => {
    expect(view('').textContent).toBe('');
  });
});
