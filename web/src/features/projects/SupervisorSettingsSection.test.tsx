import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { CLAUDE_PROFILE, CODEX_PROFILE, profile } from '@/test/fixtures/tasks';
import { SupervisorSettingsSection } from './SupervisorSettingsSection';
import { supervisorSettings } from './test/fixtures';
import {
  providersReadWith,
  supervisorSettingsRead,
  supervisorSettingsWrite
} from './test/handlers';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const providers = (list = [CLAUDE_PROFILE, CODEX_PROFILE]) =>
  providersReadWith(() => HttpResponse.json(list));

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  render(
    <QueryClientProvider client={queryClient}>
      <SupervisorSettingsSection projectId="project-a" />
    </QueryClientProvider>
  );
}

const section = () => within(screen.getByTestId('supervisor-settings'));
const picker = (name: string) => section().getByRole('combobox', { name });
const choices = (name: string) =>
  within(picker(name))
    .getAllByRole('option')
    .map((option) => option.textContent);

describe('the supervisor’s settings', () => {
  it('says what the supervisor runs on, and offers each choice with a default', async () => {
    // Given a project with nothing of its own set: the supervisor runs on the default provider
    server.use(
      providers(),
      supervisorSettingsRead(() => HttpResponse.json(supervisorSettings()))
    );
    renderSection();

    expect(await section().findByTestId('supervisor-settings-summary')).toHaveTextContent(
      'Runs on Claude, the project’s default provider.'
    );
    // Then the provider picker names the default and lists the providers that can run a supervisor
    await waitFor(() =>
      expect(choices('Provider')).toEqual(['Project default (Claude)', 'Claude', 'Codex'])
    );
    // And the other two list what the provider in force offers, by the names the owner knows
    expect(choices('Model')).toEqual(['Default', 'Sonnet 5', 'Opus 5']);
    expect(choices('Effort')).toEqual(['Default', 'low', 'medium', 'high', 'xhigh', 'max']);
    // And there is nothing to save yet
    expect(section().getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(section().queryByRole('button', { name: 'Undo changes' })).toBeNull();
  });

  it('leaves out a provider that is disabled or has no supervisor agent', async () => {
    server.use(
      providers([
        CLAUDE_PROFILE,
        profile({ id: 'off', display_name: 'Off', enabled: false }),
        profile({
          id: 'child-only',
          display_name: 'Child only',
          agents: { child: { namespace: 'agents', name: 'x-child' } }
        })
      ]),
      supervisorSettingsRead(() => HttpResponse.json(supervisorSettings()))
    );
    renderSection();

    await waitFor(() =>
      expect(choices('Provider')).toEqual(['Project default (Claude)', 'Claude'])
    );
  });

  it('names the provider in force by its own name even when it can no longer run a supervisor', async () => {
    // Given the project's own provider has since lost its supervisor agent
    const retired = profile({
      id: 'retired',
      display_name: 'Retired',
      agents: { child: { namespace: 'agents', name: 'r-child' } },
      models: [{ id: 'r-1', label: 'R One' }]
    });
    server.use(
      providers([CLAUDE_PROFILE, retired]),
      supervisorSettingsRead(() =>
        HttpResponse.json(
          supervisorSettings({
            provider_profile_id: 'retired',
            effective_provider_profile_id: 'retired',
            allowed_models: ['r-1'],
            allowed_effort_levels: []
          })
        )
      )
    );
    renderSection();

    // Then it is still named, kept in its picker and marked, and its models keep their labels
    expect(await section().findByTestId('supervisor-settings-summary')).toHaveTextContent(
      'Runs on Retired, this project’s own choice.'
    );
    await waitFor(() =>
      expect(choices('Provider')).toEqual([
        'Project default',
        'Claude',
        'Retired (cannot run a supervisor)'
      ])
    );
    expect(picker('Provider')).toHaveValue('retired');
    expect(choices('Model')).toEqual(['Default', 'R One']);
  });

  it('saves a model and an effort together, and shows the answer without a second read', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    let reads = 0;
    server.use(
      providers(),
      supervisorSettingsRead(() => {
        reads += 1;
        return HttpResponse.json(supervisorSettings());
      }),
      supervisorSettingsWrite(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(supervisorSettings({ model: 'claude-opus-5', effort: 'high' }));
      })
    );
    renderSection();
    await waitFor(() => expect(choices('Model')).toContain('Opus 5'));

    // When the owner picks a model and an effort, and saves once
    await user.selectOptions(picker('Model'), 'claude-opus-5');
    await user.selectOptions(picker('Effort'), 'high');
    // A change of model or effort alone does not restart anything, so there is no warning
    expect(section().queryByTestId('supervisor-settings-restart-warning')).toBeNull();
    await user.click(section().getByRole('button', { name: 'Save' }));

    // Then all three are sent, the provider still the default
    await waitFor(() =>
      expect(bodies).toEqual([
        { provider_profile_id: null, model: 'claude-opus-5', effort: 'high' }
      ])
    );
    await waitFor(() => expect(picker('Model')).toHaveValue('claude-opus-5'));
    expect(section().getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(section().queryByTestId('supervisor-settings-restarting')).toBeNull();
    expect(reads).toBe(1);
  });

  it('warns that another provider restarts the supervisor, drops what that provider lacks, and says it is restarting', async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(
      providers(),
      supervisorSettingsRead(() =>
        HttpResponse.json(supervisorSettings({ model: 'claude-opus-5', effort: 'xhigh' }))
      ),
      supervisorSettingsWrite(async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(
          supervisorSettings({
            provider_profile_id: 'codex-default',
            effective_provider_profile_id: 'codex-default',
            allowed_models: ['gpt-6.1-sol'],
            allowed_effort_levels: ['minimal', 'low', 'medium', 'high'],
            operation_id: 'operation-9'
          })
        );
      })
    );
    renderSection();
    await waitFor(() => expect(picker('Model')).toHaveValue('claude-opus-5'));

    // When the owner picks Codex, which offers neither that model nor that effort
    await user.selectOptions(picker('Provider'), 'codex-default');

    // Then both go back to the default, the pickers list Codex's own, and the restart is spelled out
    expect(picker('Model')).toHaveValue('');
    expect(picker('Effort')).toHaveValue('');
    expect(choices('Model')).toEqual(['Default', 'GPT-6.1 Sol']);
    expect(choices('Effort')).toEqual(['Default', ...CODEX_PROFILE.effort_levels]);
    expect(section().getByTestId('supervisor-settings-restart-warning')).toHaveTextContent(
      'Saving this restarts the supervisor on the new provider. Its workers keep running.'
    );

    await user.click(section().getByRole('button', { name: 'Save' }));

    await waitFor(() =>
      expect(bodies).toEqual([{ provider_profile_id: 'codex-default', model: null, effort: null }])
    );
    // And once the server has started the handoff, the section says so
    expect(await section().findByTestId('supervisor-settings-restarting')).toHaveTextContent(
      'The supervisor is restarting on Codex. Its workers keep running.'
    );
    expect(section().getByTestId('supervisor-settings-summary')).toHaveTextContent(
      'Runs on Codex, this project’s own choice.'
    );
  });

  it('keeps a model the new provider also lists', async () => {
    const user = userEvent.setup();
    const shared = profile({
      id: 'other',
      display_name: 'Other',
      models: [{ id: 'claude-opus-5', label: 'Opus 5' }],
      effort_levels: ['high']
    });
    server.use(
      providers([CLAUDE_PROFILE, shared]),
      supervisorSettingsRead(() =>
        HttpResponse.json(supervisorSettings({ model: 'claude-opus-5', effort: 'max' }))
      )
    );
    renderSection();
    await waitFor(() => expect(picker('Model')).toHaveValue('claude-opus-5'));

    await user.selectOptions(picker('Provider'), 'other');

    expect(picker('Model')).toHaveValue('claude-opus-5');
    expect(picker('Effort')).toHaveValue('');
  });

  it('offers only the defaults for a project default that is not the provider in force', async () => {
    const user = userEvent.setup();
    // Given the project has chosen Codex itself, so its default provider is not named by the view
    server.use(
      providers(),
      supervisorSettingsRead(() =>
        HttpResponse.json(
          supervisorSettings({
            provider_profile_id: 'codex-default',
            model: 'gpt-6.1-sol',
            effective_provider_profile_id: 'codex-default',
            allowed_models: ['gpt-6.1-sol'],
            allowed_effort_levels: ['low', 'high']
          })
        )
      )
    );
    renderSection();
    await waitFor(() => expect(picker('Provider')).toHaveValue('codex-default'));
    expect(choices('Provider')[0]).toBe('Project default');

    await user.selectOptions(picker('Provider'), '');

    expect(picker('Model')).toBeDisabled();
    expect(picker('Effort')).toBeDisabled();
    expect(picker('Model')).toHaveValue('');
    expect(
      section().getByText(
        'The project default’s models and efforts can be chosen once it is saved as the provider.'
      )
    ).toBeInTheDocument();
    // Undo puts the saved choice back.
    await user.click(section().getByRole('button', { name: 'Undo changes' }));
    expect(picker('Provider')).toHaveValue('codex-default');
    expect(picker('Model')).toHaveValue('gpt-6.1-sol');
  });

  it.each([
    [
      { reason: 'operation_pending' },
      'The supervisor is already restarting or changing provider. Try again when that has finished. Nothing was changed.'
    ],
    [
      { reason: 'task_settling' },
      'The supervisor is still starting or stopping. Try again in a moment. Nothing was changed.'
    ],
    [
      { reason: 'handoff_limit', handoffs: 3, limit: 3, window: '1h' },
      'The supervisor has been restarted 3 times in the last hour, which is the limit. Try again later. Nothing was changed.'
    ]
  ])('says in words why a provider change was refused: %j', async (detail, text) => {
    const user = userEvent.setup();
    server.use(
      providers(),
      supervisorSettingsRead(() => HttpResponse.json(supervisorSettings())),
      supervisorSettingsWrite(() => HttpResponse.json({ detail }, { status: 409 }))
    );
    renderSection();
    await waitFor(() => expect(choices('Provider')).toContain('Codex'));

    await user.selectOptions(picker('Provider'), 'codex-default');
    await user.click(section().getByRole('button', { name: 'Save' }));

    expect(await section().findByRole('alert')).toHaveTextContent(text);
    // What the owner chose is still there, and what is saved is unchanged.
    expect(picker('Provider')).toHaveValue('codex-default');
    expect(section().getByTestId('supervisor-settings-summary')).toHaveTextContent(
      'Runs on Claude, the project’s default provider.'
    );
  });

  it('says it could not load, with a way to try again', async () => {
    const user = userEvent.setup();
    let fail = true;
    server.use(
      providers(),
      supervisorSettingsRead(() =>
        fail
          ? HttpResponse.json({ detail: { reason: 'project_not_found' } }, { status: 404 })
          : HttpResponse.json(supervisorSettings())
      )
    );
    renderSection();

    expect(
      await section().findByText('Could not load the supervisor’s settings.')
    ).toBeInTheDocument();
    fail = false;
    await user.click(section().getByRole('button', { name: 'Try again' }));
    expect(await section().findByTestId('supervisor-settings-summary')).toBeInTheDocument();
  });
});
