import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import { dotfilesHandlers } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { OwnerDotfilesSection, ProjectDotfilesSection } from './DotfilesSection';
import { sourceUrl } from './model';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

function renderWith(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  return render(<QueryClientProvider client={queryClient}>{node}</QueryClientProvider>);
}

function form() {
  const section = screen.getByTestId('dotfiles');
  return {
    section,
    repository: within(section).getByRole('textbox', { name: 'Repository' }),
    ref: within(section).getByRole('textbox', { name: 'Branch, tag or commit' }),
    path: within(section).getByRole('textbox', { name: 'Directory' }),
    save: within(section).getByRole('button', { name: /Save|Saving…/ })
  };
}

const SET = { repository: 'example/agent-dotfiles', ref: 'main', path: 'home/.agents', version: 3 };

describe('owner dotfiles section', () => {
  it('says when no source is set and saves a new one', async () => {
    let body: unknown;
    server.use(
      http.put(`${API_URL}/dotfiles`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ...SET, version: 1 });
      }),
      ...dotfilesHandlers()
    );
    const user = userEvent.setup();
    renderWith(<OwnerDotfilesSection />);
    expect(
      await screen.findByText('No source is set: sessions get no dotfiles.')
    ).toBeInTheDocument();
    expect(form().save).toBeDisabled();

    await user.type(form().repository, 'example/agent-dotfiles');
    await user.type(form().ref, 'main');
    await user.type(form().path, 'home/.agents');
    await user.click(form().save);

    const link = await screen.findByRole('link', {
      name: 'example/agent-dotfiles @ main, home/.agents/'
    });
    expect(link).toHaveAttribute(
      'href',
      'https://github.com/example/agent-dotfiles/tree/main/home/.agents'
    );
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(body).toEqual({
      repository: 'example/agent-dotfiles',
      ref: 'main',
      path: 'home/.agents',
      expected_version: 0
    });
  });

  it('refuses a repository that is not owner/name before saving', async () => {
    server.use(...dotfilesHandlers());
    const user = userEvent.setup();
    renderWith(<OwnerDotfilesSection />);
    await screen.findByText(/No source is set/);
    await user.type(form().repository, 'https://github.com/example/agent-dotfiles');
    expect(screen.getByText('Write the repository as owner/name.')).toBeInTheDocument();
    expect(form().save).toBeDisabled();
  });

  it('removes the source with the version it read', async () => {
    let body: unknown;
    server.use(
      http.get(`${API_URL}/dotfiles`, () => HttpResponse.json(SET)),
      http.put(`${API_URL}/dotfiles`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ repository: null, ref: '', path: '', version: 4 });
      })
    );
    const user = userEvent.setup();
    renderWith(<OwnerDotfilesSection />);
    await screen.findByRole('link', { name: /example\/agent-dotfiles/ });
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText(/No source is set/)).toBeInTheDocument();
    expect(body).toEqual({ repository: null, ref: '', path: '', expected_version: 3 });
  });

  it('asks for a reload when the source changed elsewhere', async () => {
    server.use(
      http.get(`${API_URL}/dotfiles`, () => HttpResponse.json(SET)),
      http.put(`${API_URL}/dotfiles`, () =>
        HttpResponse.json({ detail: { reason: 'stale_dotfiles_source' } }, { status: 409 })
      )
    );
    const user = userEvent.setup();
    renderWith(<OwnerDotfilesSection />);
    await screen.findByRole('link', { name: /example\/agent-dotfiles/ });
    await user.clear(form().ref);
    await user.type(form().ref, 'v2');
    await user.click(form().save);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Not saved: the source changed elsewhere. Reload and try again.'
    );
  });

  it('refuses a source that does not match the contract', async () => {
    server.use(http.get(`${API_URL}/dotfiles`, () => HttpResponse.json({ repository: 7 })));
    renderWith(<OwnerDotfilesSection />);
    expect(
      await screen.findByText('Mainloop sent a dotfiles source that does not match the contract.')
    ).toBeInTheDocument();
  });
});

describe('project dotfiles section', () => {
  it('names the owner-wide source the project falls back to', async () => {
    server.use(...dotfilesHandlers({ owner: SET }));
    renderWith(<ProjectDotfilesSection projectId="project-a" />);
    expect(
      await screen.findByText(
        'This project uses the owner-wide source: example/agent-dotfiles @ main, home/.agents/.'
      )
    ).toBeInTheDocument();
  });

  it('sets the project’s own source and reverts to the owner-wide one', async () => {
    server.use(...dotfilesHandlers({ owner: SET }));
    const user = userEvent.setup();
    renderWith(<ProjectDotfilesSection projectId="project-a" />);
    await screen.findByText(/uses the owner-wide source/);
    await user.type(form().repository, 'example/project-dotfiles');
    await user.click(form().save);
    expect(
      await screen.findByRole('link', { name: 'example/project-dotfiles @ default branch' })
    ).toHaveAttribute('href', 'https://github.com/example/project-dotfiles');
    await user.click(screen.getByRole('button', { name: 'Use the owner-wide source' }));
    expect(await screen.findByText(/uses the owner-wide source/)).toBeInTheDocument();
  });
});

describe('sourceUrl', () => {
  it('links only a valid repository, and encodes the ref and path', () => {
    expect(sourceUrl({ repository: 'x/y"><script>', ref: '', path: '', version: 1 })).toBeNull();
    expect(sourceUrl({ repository: 'x/y', ref: 'feat/a b', path: 'd/e f', version: 1 })).toBe(
      'https://github.com/x/y/tree/feat%2Fa%20b/d/e%20f'
    );
    expect(sourceUrl({ repository: 'x/y', ref: '', path: 'd', version: 1 })).toBe(
      'https://github.com/x/y/tree/HEAD/d'
    );
  });
});
