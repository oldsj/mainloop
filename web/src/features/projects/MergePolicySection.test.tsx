import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { $api, connection } from '@/api';
import { server } from '@/test/msw/server';
import { MergePolicySection } from './MergePolicySection';
import { policy, type PolicyFixture } from './test/fixtures';
import { policyRead, policyWrite, problem } from './test/handlers';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

function renderSection(projectId = 'project-a') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MergePolicySection projectId={projectId} />
    </QueryClientProvider>
  );
  return { ...view, queryClient };
}

const policyKey = $api.queryOptions('get', '/projects/{project_id}/merge-policy', {
  params: { path: { project_id: 'project-a' } }
}).queryKey;

/** The select and the save button, found inside the section so the page's own controls are not matched. */
function controls() {
  const section = screen.getByTestId('merge-policy');
  return {
    select: within(section).queryByRole('combobox', { name: 'Policy' }),
    save: within(section).queryByRole('button', { name: /Save policy|Saving…/ })
  };
}

describe('merge policy section', () => {
  it('shows the current policy in words, the explanation and the read-only protected globs', async () => {
    server.use(
      policyRead(() =>
        HttpResponse.json(policy({ writes_enabled: false, merge_policy: 'approval' }))
      )
    );
    const user = userEvent.setup();
    renderSection();

    expect(await screen.findByText('Current policy: Approval required')).toBeInTheDocument();
    expect(
      screen.getByText(
        'Auto permits eligible merges after server checks. Approval requires an owner decision. Protected paths always require approval.'
      )
    ).toBeInTheDocument();
    expect(
      screen.getByText('Editing disabled by the server. The current policy remains in effect.')
    ).toBeInTheDocument();

    const { select, save } = controls();
    expect(select).toBeDisabled();
    expect(save).toBeDisabled();

    await user.click(screen.getByText('Protected paths (read-only)'));
    expect(screen.getByText('k8s/**')).toBeInTheDocument();
    expect(screen.getByText('docs/specs/**')).toBeInTheDocument();
  });

  it('enables the select only when the server accepts writes, and saves only a changed policy', async () => {
    server.use(
      policyRead(() => HttpResponse.json(policy({ writes_enabled: true, merge_policy: 'auto' })))
    );
    const user = userEvent.setup();
    renderSection();

    await screen.findByText('Current policy: Auto');
    const { select, save } = controls();
    expect(select).toBeEnabled();
    expect(save).toBeDisabled();

    await user.selectOptions(select!, 'approval');
    expect(controls().save).toBeEnabled();
  });

  it('sends the chosen policy with the version it was read at, then shows the re-read result', async () => {
    const bodies: Record<string, unknown>[] = [];
    let current = policy({ writes_enabled: true, merge_policy: 'auto', merge_policy_version: 3 });
    server.use(
      policyRead(() => HttpResponse.json(current)),
      policyWrite(async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        current = policy({
          writes_enabled: true,
          merge_policy: 'approval',
          merge_policy_version: 4
        });
        return HttpResponse.json(current);
      })
    );
    const user = userEvent.setup();
    renderSection();

    await screen.findByText('Current policy: Auto');
    await user.selectOptions(controls().select!, 'approval');
    await user.click(controls().save!);

    expect(await screen.findByText('Current policy: Approval required')).toBeInTheDocument();
    expect(bodies).toEqual([{ merge_policy: 'approval', expected_version: 3 }]);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('does not submit twice while a save is in flight', async () => {
    let posts = 0;
    let current = policy({ writes_enabled: true, merge_policy: 'auto' });
    server.use(
      policyRead(() => HttpResponse.json(current)),
      policyWrite(async () => {
        posts += 1;
        await delay(200);
        current = policy({
          writes_enabled: true,
          merge_policy: 'approval',
          merge_policy_version: 4
        });
        return HttpResponse.json(current);
      })
    );
    const user = userEvent.setup();
    renderSection();

    await screen.findByText('Current policy: Auto');
    await user.selectOptions(controls().select!, 'approval');
    await user.click(controls().save!);

    const pending = await screen.findByRole('button', { name: 'Saving…' });
    expect(pending).toBeDisabled();
    await user.click(pending);

    expect(await screen.findByText('Current policy: Approval required')).toBeInTheDocument();
    expect(posts).toBe(1);
  });

  it('a refused save keeps the server reason, locks the form, and offers a refresh', async () => {
    let reads = 0;
    server.use(
      policyRead(() => {
        reads += 1;
        return HttpResponse.json(policy({ writes_enabled: true, merge_policy: 'auto' }));
      }),
      policyWrite(problem(503, 'Owner policy writes are off on this server'))
    );
    const user = userEvent.setup();
    renderSection();

    await screen.findByText('Current policy: Auto');
    await user.selectOptions(controls().select!, 'approval');
    await user.click(controls().save!);

    // The server's reason is shown, not replaced with a generic message.
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'Owner policy writes are off on this server. Review the current policy before saving again.'
    );
    expect(within(alert).getByRole('button', { name: 'Refresh policy' })).toBeEnabled();
    // The save is held until the owner refreshes, so the stale draft cannot be sent again.
    expect(controls().select).toBeDisabled();
    expect(controls().save).toBeDisabled();
    // The error survives the policy re-read that follows a failed save.
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Owner policy writes are off on this server'
    );
    expect(reads).toBeGreaterThanOrEqual(2);
  });

  it('refreshing after a refused save clears the error and remounts the form from the new policy', async () => {
    let current = policy({ writes_enabled: true, merge_policy: 'auto', merge_policy_version: 3 });
    server.use(
      policyRead(() => HttpResponse.json(current)),
      policyWrite(problem(503, 'Owner policy writes are off on this server'))
    );
    const user = userEvent.setup();
    renderSection();

    await screen.findByText('Current policy: Auto');
    await user.selectOptions(controls().select!, 'approval');
    await user.click(controls().save!);
    const alert = await screen.findByRole('alert');

    current = policy({ writes_enabled: true, merge_policy: 'approval', merge_policy_version: 4 });
    await user.click(within(alert).getByRole('button', { name: 'Refresh policy' }));

    expect(await screen.findByText('Current policy: Approval required')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(controls().select).toHaveValue('approval');
    expect(controls().select).toBeEnabled();
    expect(controls().save).toBeDisabled();
  });

  it('a failed first read shows a refresh control and no form', async () => {
    server.use(policyRead(problem(500, 'Merge policy is unavailable')));
    renderSection();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Could not load the current policy. Refresh before editing.');
    expect(alert).toHaveTextContent('Merge policy is unavailable');
    expect(within(alert).getByRole('button', { name: 'Refresh policy' })).toBeInTheDocument();
    expect(controls().select).toBeNull();
  });

  it('a policy that breaks the contract is not shown as a policy', async () => {
    const broken = { ...policy(), merge_policy: 'mystery' } as unknown as PolicyFixture;
    server.use(policyRead(() => HttpResponse.json(broken)));
    renderSection();

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Mainloop sent a merge policy that does not match the contract.'
    );
    expect(controls().select).toBeNull();
    expect(screen.queryByText(/Current policy/)).not.toBeInTheDocument();
  });

  it('a failed refresh keeps the loaded policy with an inline error and a refresh control', async () => {
    let failing = false;
    server.use(
      policyRead(() =>
        failing
          ? HttpResponse.json({ detail: 'Policy read failed' }, { status: 500 })
          : HttpResponse.json(policy({ writes_enabled: false, merge_policy: 'auto' }))
      )
    );
    const user = userEvent.setup();
    const { queryClient } = renderSection();
    await screen.findByText('Current policy: Auto');

    failing = true;
    await queryClient.invalidateQueries({ queryKey: policyKey });
    const block = await screen.findByTestId('merge-policy-refresh-error');
    expect(block).toHaveTextContent(
      'Could not refresh the current policy. Refresh before editing.'
    );
    expect(block).toHaveTextContent('Policy read failed');
    expect(screen.getByText('Current policy: Auto')).toBeInTheDocument();

    failing = false;
    await user.click(within(block).getByRole('button', { name: 'Refresh policy' }));
    expect(screen.queryByTestId('merge-policy-refresh-error')).not.toBeInTheDocument();
  });

  it('a failed refresh after a refused save keeps the form locked until a refresh succeeds', async () => {
    let failing = false;
    const current = policy({ writes_enabled: true, merge_policy: 'auto', merge_policy_version: 3 });
    server.use(
      policyRead(() =>
        failing
          ? HttpResponse.json({ detail: 'Policy read failed' }, { status: 500 })
          : HttpResponse.json(current)
      ),
      policyWrite(problem(503, 'Owner policy writes are off on this server'))
    );
    const user = userEvent.setup();
    renderSection();

    await screen.findByText('Current policy: Auto');
    await user.selectOptions(controls().select!, 'approval');
    await user.click(controls().save!);
    const alert = await screen.findByRole('alert');

    // The re-read fails, so the draft is still not known to be current. The lock must hold.
    failing = true;
    await user.click(within(alert).getByRole('button', { name: 'Refresh policy' }));
    const block = await screen.findByTestId('merge-policy-refresh-error');
    expect(block).toHaveTextContent('Policy read failed');
    expect(controls().select).toBeDisabled();
    expect(controls().save).toBeDisabled();

    failing = false;
    await user.click(within(block).getByRole('button', { name: 'Refresh policy' }));
    await waitFor(() =>
      expect(screen.queryByTestId('merge-policy-refresh-error')).not.toBeInTheDocument()
    );
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(controls().select).toBeEnabled();
  });
});
