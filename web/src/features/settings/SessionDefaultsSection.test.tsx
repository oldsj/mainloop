import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import { CLAUDE_PROFILE, CODEX_PROFILE } from '@/features/chat/handlers';
import { sessionDefaultsHandlers } from '@/test/msw/handlers';
import { server } from '@/test/msw/server';
import { SessionDefaultsSection } from './SessionDefaultsSection';

type SessionDefault = components['schemas']['SessionDefault'];
type ProviderProfile = components['schemas']['ProviderProfile'];

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const url = (path: string) => `${API_URL}${path}`;

function serve(
  defaults: SessionDefault[] = [],
  profiles: ProviderProfile[] = [CLAUDE_PROFILE, CODEX_PROFILE]
) {
  const puts: SessionDefault[] = [];
  server.use(
    http.get(url('/providers'), () => HttpResponse.json(profiles)),
    // Recorded first, then answered by the in-memory handlers below.
    http.put(url('/session-defaults'), async ({ request }) => {
      puts.push((await request.clone().json()) as SessionDefault);
      return undefined;
    }),
    ...sessionDefaultsHandlers(defaults)
  );
  return puts;
}

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  render(
    <QueryClientProvider client={queryClient}>
      <SessionDefaultsSection />
    </QueryClientProvider>
  );
  return userEvent.setup();
}

const field = (name: string) => screen.findByRole('combobox', { name });

describe('session defaults', () => {
  it('lists a model and an effort for each role and enabled provider', async () => {
    serve([], [CLAUDE_PROFILE, { ...CODEX_PROFILE, enabled: false }]);
    renderSection();
    const main = await screen.findByRole('group', { name: 'Main thread' });
    const workers = screen.getByRole('group', { name: 'Workers' });
    for (const group of [main, workers]) {
      expect(within(group).getAllByRole('combobox')).toHaveLength(2);
      expect(within(group).getByText('Claude Code')).toBeInTheDocument();
      expect(within(group).queryByText('Codex')).toBeNull();
    }
  });

  it('shows what is saved, and names the provider default for what is not', async () => {
    serve([{ role: 'worker', provider: 'claude', model: 'claude-opus-5', effort: null }]);
    renderSection();
    expect(await field('Workers, Claude Code: model')).toHaveValue('claude-opus-5');
    const effort = await field('Workers, Claude Code: effort');
    expect(effort).toHaveValue('');
    expect(
      within(effort).getByRole('option', { name: 'Provider default (medium)' })
    ).toBeInTheDocument();
    const mainModel = await field('Main thread, Claude Code: model');
    expect(mainModel).toHaveValue('');
    expect(
      within(mainModel).getByRole('option', { name: 'Provider default (Sonnet 5)' })
    ).toBeInTheDocument();
  });

  it('saves a choice as it is made, for that role and provider only, and says so', async () => {
    const puts = serve();
    const user = renderSection();
    await user.selectOptions(await field('Workers, Claude Code: model'), 'claude-opus-5');
    await waitFor(() =>
      expect(puts).toEqual([
        { role: 'worker', provider: 'claude', model: 'claude-opus-5', effort: null }
      ])
    );
    expect(await screen.findByTestId('session-default-worker-claude-notice')).toHaveTextContent(
      'Saved.'
    );
    expect(await field('Workers, Claude Code: model')).toHaveValue('claude-opus-5');
    expect(await field('Main thread, Claude Code: model')).toHaveValue('');

    // Effort is saved with the model already chosen, so neither field undoes the other.
    await user.selectOptions(await field('Workers, Claude Code: effort'), 'high');
    await waitFor(() =>
      expect(puts.at(-1)).toEqual({
        role: 'worker',
        provider: 'claude',
        model: 'claude-opus-5',
        effort: 'high'
      })
    );
  });

  it('offers each provider its own effort levels', async () => {
    serve();
    renderSection();
    const codex = await field('Workers, Codex: effort');
    expect(within(codex).getByRole('option', { name: 'minimal' })).toBeInTheDocument();
    expect(within(codex).queryByRole('option', { name: 'max' })).toBeNull();
    const claude = await field('Workers, Claude Code: effort');
    expect(within(claude).getByRole('option', { name: 'max' })).toBeInTheDocument();
  });

  it('removes a default when both fields go back to the provider', async () => {
    const puts = serve([
      { role: 'main', provider: 'claude', model: 'claude-opus-5', effort: null }
    ]);
    const user = renderSection();
    await user.selectOptions(await field('Main thread, Claude Code: model'), '');
    await waitFor(() =>
      expect(puts).toEqual([{ role: 'main', provider: 'claude', model: null, effort: null }])
    );
    expect(await field('Main thread, Claude Code: model')).toHaveValue('');
  });

  it('says a refused save was not saved, and keeps what was saved before', async () => {
    serve([{ role: 'worker', provider: 'codex', model: null, effort: 'low' }]);
    server.use(
      http.put(url('/session-defaults'), () =>
        HttpResponse.json(
          { detail: 'effort "high" is not offered by provider "codex"' },
          { status: 422 }
        )
      )
    );
    const user = renderSection();
    await user.selectOptions(await field('Workers, Codex: effort'), 'high');
    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent(/^Not saved/);
    expect(await field('Workers, Codex: effort')).toHaveValue('low');
  });

  it('says so when a saved effort is no longer offered', async () => {
    serve([{ role: 'worker', provider: 'codex', model: null, effort: 'max' }]);
    renderSection();
    expect(await field('Workers, Codex: effort')).toHaveValue('max');
    expect(
      screen.getByText(/Codex no longer offers “max”, so turns leave the effort to the provider/)
    ).toBeInTheDocument();
  });

  it('offers a retry when the defaults cannot be read', async () => {
    serve();
    server.use(
      http.get(url('/session-defaults'), () =>
        HttpResponse.json({ detail: 'nope' }, { status: 500 })
      )
    );
    renderSection();
    expect(await screen.findByText('Could not load the session defaults')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Retry|Try again/ })).toBeInTheDocument();
  });

  it('says there is nothing to set when no provider is enabled', async () => {
    serve([], []);
    renderSection();
    expect(
      await screen.findByText(
        'No provider is configured, so there is nothing to set a default for.'
      )
    ).toBeInTheDocument();
  });
});
