import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import { delay, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { $api, connection } from '@/api';
import type { VersionRow } from '@/features/environments/model';
import {
  detailRead,
  policyRead,
  providersRead,
  workspacesRead
} from '@/features/projects/test/handlers';
import { detail, policy, project } from '@/features/projects/test/fixtures';
import { renderProjects } from '@/features/projects/test/render';
import { server } from '@/test/msw/server';
import { EnvironmentSection } from './EnvironmentSection';
import {
  environment,
  selection,
  version,
  type EnvironmentFixture,
  type SelectionFixture,
  type VersionFixture
} from './test/fixtures';
import {
  acceptDefaultWrite,
  environmentsRead,
  environmentsReadWith,
  registerWrite,
  savedSelection,
  selectionRead,
  selectionWrite,
  serveEnvironments,
  versionsRead,
  versionsReadWith
} from './test/handlers';

// The section makes several reads at once, and past the limiter's burst they queue. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const IMAGE = `ghcr.io/example/tools@sha256:${'a'.repeat(64)}`;

function renderSection(projectId = 'project-a') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <EnvironmentSection projectId={projectId} />
    </QueryClientProvider>
  );
  return { ...view, queryClient };
}

async function bodyOf(request: Request): Promise<Record<string, unknown>> {
  return (await request.json()) as Record<string, unknown>;
}

/** The saved selection a successful write leaves behind, at the revision the server would assign. */
function savedFrom(body: Record<string, unknown>, revision: number): SelectionFixture {
  const follow = Boolean(body.follow_default);
  return selection({
    environment_id: String(body.environment_id),
    version_id: (body.version_id as string | null) ?? null,
    follow_default: follow,
    revision,
    resolved_version_id: follow ? 'v2' : ((body.version_id as string | null) ?? null)
  });
}

/** Waits for the environments to load, then picks one. The select renders before its options do. */
async function chooseEnvironment(user: UserEvent, id: string) {
  const select = await screen.findByRole('combobox', { name: 'Environment' });
  await within(select).findByRole('option', { name: 'Python tools' });
  await user.selectOptions(select, id);
}

/** Waits for a version to be offered, then picks it. Versions load after the environment is chosen. */
async function chooseVersion(user: UserEvent, id: string) {
  const select = await screen.findByRole('combobox', { name: 'Version' });
  await within(select).findByRole('option', { name: new RegExp(`^${id} ·`) });
  await user.selectOptions(select, id);
}

describe('environment selection', () => {
  it('first selection uses observed null revision and exact fixed/follow choices', async () => {
    const writes: Record<string, unknown>[] = [];
    let saved: SelectionFixture | null = null;
    server.use(
      environmentsRead([environment({ accepted_default_version_id: 'v2' })]),
      versionsRead({ env: [version({ id: 'v2' })] }),
      selectionRead(() =>
        saved ? HttpResponse.json(saved) : new HttpResponse(null, { status: 204 })
      ),
      selectionWrite(async ({ request }) => {
        const body = await bodyOf(request);
        writes.push(body);
        saved = savedFrom(body, (saved?.revision ?? 0) + 1);
        return HttpResponse.json(saved);
      })
    );
    const user = userEvent.setup();
    renderSection();

    await chooseEnvironment(user, 'env');
    await chooseVersion(user, 'v2');
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(await screen.findByText('Saved: Python tools · Fixed version · v2')).toBeInTheDocument();
    expect(writes[0]).toEqual({
      environment_id: 'env',
      version_id: 'v2',
      follow_default: false,
      expected_version: 0
    });

    // Wait for the saved line to show revision 1, so the second save sends the revision the server holds.
    await user.click(screen.getByRole('checkbox', { name: /Follow this environment/ }));
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(
      await screen.findByText('Saved: Python tools · Follow accepted default')
    ).toBeInTheDocument();
    expect(writes[1]).toEqual({
      environment_id: 'env',
      version_id: null,
      follow_default: true,
      expected_version: 1
    });
  });

  it('conflict refreshes observed revision while retaining the owner draft; no retry', async () => {
    let reads = 0;
    const puts: Record<string, unknown>[] = [];
    server.use(
      environmentsRead([environment({ accepted_default_version_id: 'v2' })]),
      versionsRead({ env: [version({ id: 'v2' }), version({ id: 'v3' })] }),
      selectionRead(() => {
        reads += 1;
        return HttpResponse.json(
          selection({
            version_id: 'other',
            revision: reads === 1 ? 3 : 4,
            resolved_version_id: 'other'
          })
        );
      }),
      selectionWrite(async ({ request }) => {
        puts.push(await bodyOf(request));
        return HttpResponse.json({ detail: 'Stale expected version' }, { status: 409 });
      })
    );
    const user = userEvent.setup();
    renderSection();

    await chooseVersion(user, 'v2');
    await user.click(screen.getByRole('button', { name: 'Save selection' }));

    expect(await screen.findByText(/Review the refreshed/)).toBeInTheDocument();
    expect(screen.getByText('Saved: Python tools · Fixed version · other')).toBeInTheDocument();
    expect(screen.getByLabelText('Version')).toHaveValue('v2');
    expect(puts).toHaveLength(1);
    expect(puts[0]).toMatchObject({ expected_version: 3 });
    expect(reads).toBe(2);
  });

  it('a late old-route response cannot enable or replace current route state', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let startedA = 0;
    server.use(
      detailRead(({ params }) =>
        HttpResponse.json(
          detail({
            project: project({
              id: String(params.project_id),
              full_name: `acme/${String(params.project_id)}`
            })
          })
        )
      ),
      policyRead(() => HttpResponse.json(policy())),
      workspacesRead([]),
      providersRead(),
      environmentsRead([environment({ accepted_default_version_id: 'v2' })]),
      versionsRead({ env: [version({ id: 'v2' })] }),
      selectionRead(async ({ params }) => {
        if (params.project_id !== 'project-a') return new HttpResponse(null, { status: 204 });
        startedA += 1;
        // Project A's reply arrives after the owner has moved to project B.
        await gate;
        return HttpResponse.json(
          selection({
            project_id: 'project-a',
            version_id: 'v2',
            revision: 1,
            resolved_version_id: 'v2'
          })
        );
      })
    );
    const { router, queryClient } = renderProjects('/projects/project-a');
    // Move on only once project A's selection request is in flight, so its reply really is late.
    await waitFor(() => expect(startedA).toBe(1));
    await router.navigate({ to: '/projects/$id', params: { id: 'project-b' } });
    expect(
      await screen.findByRole('heading', { level: 1, name: 'acme/project-b' })
    ).toBeInTheDocument();
    expect(await screen.findByText(/No selection\. New sessions/)).toBeInTheDocument();

    const projectAKey = $api.queryOptions('get', '/projects/{project_id}/environment', {
      params: { path: { project_id: 'project-a' } }
    }).queryKey;
    release();
    await waitFor(() => expect(queryClient.getQueryState(projectAKey)?.status).toBe('success'));

    expect(screen.getByRole('heading', { level: 1, name: 'acme/project-b' })).toBeInTheDocument();
    expect(screen.queryByText(/Saved: Python tools/)).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Environment' })).toHaveValue('');
  });

  it('pending and obsolete policy versions cannot be saved, including defaults', async () => {
    const writes: unknown[] = [];
    server.use(
      selectionWrite(async ({ request }) => {
        writes.push(await bodyOf(request));
        return HttpResponse.json({ detail: 'unexpected' }, { status: 500 });
      })
    );
    const rejected: VersionFixture[] = [
      version({ id: 'v2', validation_status: 'pending_build' }),
      version({ id: 'v2', validator_version: 'oci-static-v1' })
    ];
    const user = userEvent.setup();

    for (const candidate of rejected) {
      server.use(
        environmentsRead([environment({ accepted_default_version_id: 'v2' })]),
        versionsRead({ env: [candidate] }),
        savedSelection(null)
      );
      const { unmount } = renderSection();
      await chooseEnvironment(user, 'env');
      const option = await within(screen.getByRole('combobox', { name: 'Version' })).findByRole(
        'option',
        {
          name: /^v2 ·/
        }
      );
      expect(option).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Save selection' })).toBeDisabled();

      await user.click(screen.getByRole('checkbox', { name: /Follow this environment/ }));
      expect(
        screen.getByText(/An accepted current-policy version is required/)
      ).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Save selection' })).toBeDisabled();
      await user.click(screen.getByRole('button', { name: 'Save selection' }));
      expect(writes).toHaveLength(0);
      unmount();
    }
  });

  it('a save error stays on screen after a refresh', async () => {
    server.use(
      ...serveEnvironments({ environments: [environment()], versions: { env: [version()] } }),
      selectionWrite(async () => {
        await delay(200);
        return HttpResponse.json({ detail: 'Selection store is unavailable' }, { status: 503 });
      })
    );
    const user = userEvent.setup();
    renderSection();

    await chooseEnvironment(user, 'env');
    await chooseVersion(user, 'v1');
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(await screen.findByRole('button', { name: 'Saving…' })).toBeDisabled();

    expect(await screen.findByText('Selection store is unavailable')).toBeInTheDocument();
    await user.click(
      screen.getByRole('button', { name: 'Refresh saved selection and environments' })
    );
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Refresh saved selection and environments' })
      ).toBeEnabled()
    );
    expect(screen.getByText('Selection store is unavailable')).toBeInTheDocument();
  });

  it('lets the refresh label wrap inside a 390 px phone column', async () => {
    server.use(
      environmentsRead([environment()]),
      versionsRead({ env: [version()] }),
      selectionRead(() =>
        HttpResponse.json(selection({ version_id: 'v1', resolved_version_id: 'v1' }))
      )
    );
    renderSection();

    const refresh = await screen.findByRole('button', {
      name: 'Refresh saved selection and environments'
    });
    // The base button never wraps; the header's label may, so the button stays inside the column.
    expect(refresh).toHaveClass('max-w-full', 'whitespace-normal');
    expect(refresh).not.toHaveClass('whitespace-nowrap');
  });

  it('a failed refresh keeps the last saved selection with a retry', async () => {
    let failing = false;
    server.use(
      environmentsRead([environment()]),
      versionsRead({ env: [version()] }),
      selectionRead(() =>
        failing
          ? HttpResponse.json({ detail: 'Selection read failed' }, { status: 500 })
          : HttpResponse.json(selection({ version_id: 'v1', resolved_version_id: 'v1' }))
      )
    );
    const user = userEvent.setup();
    renderSection();

    expect(await screen.findByText('Saved: Python tools · Fixed version · v1')).toBeInTheDocument();
    failing = true;
    await user.click(
      screen.getByRole('button', { name: 'Refresh saved selection and environments' })
    );
    const block = await screen.findByTestId('environment-refresh-error');
    expect(within(block).getByRole('alert')).toHaveTextContent('Selection read failed');
    expect(within(block).getByRole('alert')).toHaveTextContent('Showing the last saved selection.');
    expect(screen.getByText('Saved: Python tools · Fixed version · v1')).toBeInTheDocument();

    failing = false;
    await user.click(within(block).getByRole('button', { name: 'Retry loading environment' }));
    await waitFor(() =>
      expect(screen.queryByTestId('environment-refresh-error')).not.toBeInTheDocument()
    );
  });

  it('owner API encodes paths and sends exact CAS/default/registration bodies; sanitized errors retain status', async () => {
    const selectionPuts: { path: string; body: Record<string, unknown> }[] = [];
    const acceptPuts: { path: string; body: Record<string, unknown> }[] = [];
    const registrations: Record<string, unknown>[] = [];
    server.use(
      environmentsRead([environment({ id: 'env/a', accepted_default_version_id: 'v1' })]),
      versionsRead({
        'env/a': [
          version({ id: 'v1', environment_id: 'env/a' }),
          version({ id: 'v2', environment_id: 'env/a' })
        ]
      }),
      savedSelection(
        selection({
          project_id: 'project/a',
          environment_id: 'env/a',
          version_id: 'v1',
          revision: 7,
          resolved_version_id: 'v1'
        })
      ),
      selectionWrite(async ({ request }) => {
        selectionPuts.push({ path: new URL(request.url).pathname, body: await bodyOf(request) });
        return HttpResponse.json({ detail: 'unexpected' }, { status: 500 });
      }),
      acceptDefaultWrite(async ({ request }) => {
        acceptPuts.push({ path: new URL(request.url).pathname, body: await bodyOf(request) });
        return HttpResponse.json({ detail: 'unexpected' }, { status: 500 });
      }),
      registerWrite(async ({ request }) => {
        registrations.push(await bodyOf(request));
        return HttpResponse.json({ detail: 'Environment use grant required' }, { status: 403 });
      })
    );
    const user = userEvent.setup();
    renderSection('project/a');

    // Following the accepted default (v1) sends the revision the owner last saw.
    await user.click(await screen.findByRole('checkbox', { name: /Follow this environment/ }));
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    await waitFor(() => expect(selectionPuts).toHaveLength(1));
    expect(selectionPuts[0]?.path.endsWith('/projects/project%2Fa/environment')).toBe(true);
    expect(selectionPuts[0]?.body).toEqual({
      environment_id: 'env/a',
      version_id: null,
      follow_default: true,
      expected_version: 7
    });

    // Accepting a chosen version encodes the environment id and sends only the version.
    await user.click(screen.getByRole('checkbox', { name: /Follow this environment/ }));
    await chooseVersion(user, 'v2');
    await user.click(
      screen.getByRole('button', { name: 'Accept selected version as environment default' })
    );
    await waitFor(() => expect(acceptPuts).toHaveLength(1));
    expect(acceptPuts[0]?.path.endsWith('/environments/env%2Fa/default')).toBe(true);
    expect(acceptPuts[0]?.body).toEqual({ version_id: 'v2' });

    // A 403 keeps its message and does not lock the form: it is a refusal, not an uncertain reply.
    await user.click(screen.getByText('Register a public image'));
    await user.type(screen.getByLabelText('Name'), 'Public');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.selectOptions(screen.getByLabelText('Architecture'), 'arm64');
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));
    expect(await screen.findByText('Environment use grant required')).toBeInTheDocument();
    expect(registrations).toEqual([
      { name: 'Public', source_kind: 'prebuilt_image', image: IMAGE, architecture: 'arm64' }
    ]);
    expect(screen.queryByText(/Registration reply was not confirmed/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Register and validate image' })).toBeEnabled();
  });

  it('registration uses the real returned status without selecting or accepting a default', async () => {
    const selectionWrites: unknown[] = [];
    const registrations: Record<string, unknown>[] = [];
    server.use(
      ...serveEnvironments(),
      selectionWrite(async ({ request }) => {
        selectionWrites.push(await bodyOf(request));
        return HttpResponse.json({ detail: 'unexpected' }, { status: 500 });
      }),
      registerWrite(async ({ request }) => {
        registrations.push(await bodyOf(request));
        return HttpResponse.json({
          environment: environment({
            id: 'registered',
            name: 'Public image',
            accepted_default_version_id: null
          }),
          version: version({ id: 'registered-version', environment_id: 'registered' })
        });
      })
    );
    const user = userEvent.setup();
    const { queryClient } = renderSection();

    await user.click(await screen.findByText('Register a public image'));
    await user.type(screen.getByLabelText('Name'), 'Public image');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));

    expect(
      await screen.findByText(
        'Registered Public image: static_validated, oci-static-v2. Runtime capabilities have not been tested.'
      )
    ).toBeInTheDocument();
    expect(registrations).toHaveLength(1);
    expect(screen.getByRole('combobox', { name: 'Environment' })).toHaveValue('');
    expect(selectionWrites).toHaveLength(0);
    // The registered environment's version is cached, so it can be chosen without another read.
    const registeredVersions = queryClient.getQueryData(
      $api.queryOptions('get', '/environments/{environment_id}/versions', {
        params: { path: { environment_id: 'registered' } }
      }).queryKey
    ) as VersionRow[] | undefined;
    expect(registeredVersions?.map((item) => item.id)).toEqual(['registered-version']);
  });

  it('uncertain registration blocks repeat POST even after a read refresh', async () => {
    let posts = 0;
    let listReads = 0;
    server.use(
      environmentsReadWith(() => {
        listReads += 1;
        return HttpResponse.json([environment()]);
      }),
      versionsRead({ env: [version()] }),
      savedSelection(null),
      registerWrite(() => {
        posts += 1;
        return HttpResponse.error();
      })
    );
    const user = userEvent.setup();
    renderSection();

    await user.click(await screen.findByText('Register a public image'));
    await user.type(screen.getByLabelText('Name'), 'Public tools');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));
    expect(await screen.findByText(/Registration reply was not confirmed/)).toBeInTheDocument();

    const readsBeforeRefresh = listReads;
    await user.click(
      screen.getByRole('button', { name: 'Refresh saved selection and environments' })
    );
    await waitFor(() => expect(listReads).toBeGreaterThan(readsBeforeRefresh));

    expect(screen.getByRole('button', { name: 'Register and validate image' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));
    expect(posts).toBe(1);
    expect(screen.getByText(/Registration reply was not confirmed/)).toBeInTheDocument();
  });

  it('server error after registration also blocks uncertain repeat POST', async () => {
    let posts = 0;
    server.use(
      ...serveEnvironments(),
      registerWrite(() => {
        posts += 1;
        return HttpResponse.json({ detail: 'Registry unavailable' }, { status: 500 });
      })
    );
    const user = userEvent.setup();
    renderSection();

    await user.click(await screen.findByText('Register a public image'));
    await user.type(screen.getByLabelText('Name'), 'Public tools');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));
    expect(await screen.findByText(/Registration reply was not confirmed/)).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));
    expect(posts).toBe(1);
    expect(screen.getByRole('button', { name: 'Register and validate image' })).toBeDisabled();
  });

  it('a 204 selection reads as no selection, not as an error', async () => {
    server.use(...serveEnvironments({ selection: null }));
    renderSection();
    expect(
      await screen.findByText('No selection. New sessions use the configured native agent image.')
    ).toBeInTheDocument();
    expect(screen.queryByTestId('environment-refresh-error')).not.toBeInTheDocument();
  });

  it('drops environment and version rows that fail the contract', async () => {
    const badEnvironment = {
      ...environment({ id: 'bad', name: 'Broken' }),
      source_kind: 'mystery'
    } as unknown as EnvironmentFixture;
    const badVersion = {
      ...version({ id: 'v-bad' }),
      validation_status: 'mystery'
    } as unknown as VersionFixture;
    server.use(
      environmentsRead([environment(), badEnvironment]),
      versionsRead({ env: [version(), badVersion] }),
      savedSelection(null)
    );
    const user = userEvent.setup();
    renderSection();

    const environmentSelect = await screen.findByRole('combobox', { name: 'Environment' });
    await within(environmentSelect).findByRole('option', { name: 'Python tools' });
    // The empty placeholder plus the one valid environment. The malformed row is not offered.
    expect(within(environmentSelect).getAllByRole('option')).toHaveLength(2);
    expect(
      within(environmentSelect).queryByRole('option', { name: 'Broken' })
    ).not.toBeInTheDocument();
    await user.selectOptions(environmentSelect, 'env');
    const versionSelect = screen.getByRole('combobox', { name: 'Version' });
    await within(versionSelect).findByRole('option', { name: /^v1 ·/ });
    expect(within(versionSelect).getAllByRole('option')).toHaveLength(2);
    expect(screen.queryByRole('option', { name: /v-bad/ })).not.toBeInTheDocument();
  });

  it('an unreadable version list says so with a retry, and does not claim there is no accepted default', async () => {
    let versionReads = 0;
    server.use(
      environmentsRead([environment({ accepted_default_version_id: 'v1' })]),
      versionsReadWith(() => {
        versionReads += 1;
        return versionReads === 1
          ? HttpResponse.json({ detail: 'Version list is unavailable' }, { status: 500 })
          : HttpResponse.json([version({ id: 'v1' })]);
      }),
      savedSelection(null)
    );
    const user = userEvent.setup();
    renderSection();

    await chooseEnvironment(user, 'env');
    const follow = screen.getByRole('checkbox', { name: /Follow this environment/ });
    await user.click(follow);
    expect(follow).toBeChecked();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load the versions for this environment.");
    expect(alert).toHaveTextContent('Version list is unavailable');
    // Without a readable list, the warning cannot say whether an accepted default exists.
    expect(
      screen.queryByText(/An accepted current-policy version is required/)
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Retry loading versions' }));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
    expect(versionReads).toBe(2);
    expect(
      screen.queryByText(/An accepted current-policy version is required/)
    ).not.toBeInTheDocument();
  });

  it('an uncertain registration is announced once', async () => {
    server.use(
      ...serveEnvironments(),
      registerWrite(() => HttpResponse.error())
    );
    const user = userEvent.setup();
    renderSection();

    await user.click(await screen.findByText('Register a public image'));
    await user.type(screen.getByLabelText('Name'), 'Public tools');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));
    await screen.findByText(/Registration reply was not confirmed/);

    const announced = screen
      .getAllByRole('alert')
      .filter((node) => /Registration reply was not confirmed/.test(node.textContent ?? ''));
    expect(announced).toHaveLength(1);
  });

  it('a successful registration clears the form and collapses it', async () => {
    let posts = 0;
    server.use(
      ...serveEnvironments(),
      registerWrite(() => {
        posts += 1;
        return HttpResponse.json({
          environment: environment({
            id: 'registered',
            name: 'Public image',
            accepted_default_version_id: null
          }),
          version: version({ id: 'registered-version', environment_id: 'registered' })
        });
      })
    );
    const user = userEvent.setup();
    renderSection();

    const toggle = await screen.findByText('Register a public image');
    await user.click(toggle);
    await user.type(screen.getByLabelText('Name'), 'Public image');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));

    expect(await screen.findByText(/^Registered Public image:/)).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveValue('');
    expect(screen.getByLabelText('Image')).toHaveValue('');
    expect(screen.getByRole('button', { name: 'Register and validate image' })).toBeDisabled();
    // RegisterForm is remounted after a registration, so the collapsed <details> is a new element.
    expect(screen.getByText('Register a public image').closest('details')).not.toHaveAttribute(
      'open'
    );
    expect(posts).toBe(1);
  });

  it('a failed registration clears the success notice from an earlier save', async () => {
    server.use(
      ...serveEnvironments(),
      selectionWrite(async ({ request }) => HttpResponse.json(savedFrom(await bodyOf(request), 1))),
      registerWrite(() => HttpResponse.json({ detail: 'Image is not public' }, { status: 400 }))
    );
    const user = userEvent.setup();
    renderSection();

    await chooseEnvironment(user, 'env');
    await chooseVersion(user, 'v1');
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(
      await screen.findByText('Environment selection saved for new sessions.')
    ).toBeInTheDocument();

    await user.click(screen.getByText('Register a public image'));
    await user.type(screen.getByLabelText('Name'), 'Public tools');
    await user.type(screen.getByLabelText('Image'), IMAGE);
    await user.click(screen.getByRole('button', { name: 'Register and validate image' }));

    expect(await screen.findByText('Image is not public')).toBeInTheDocument();
    expect(
      screen.queryByText('Environment selection saved for new sessions.')
    ).not.toBeInTheDocument();
  });
});
