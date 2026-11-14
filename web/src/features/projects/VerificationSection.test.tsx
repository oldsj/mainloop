import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { server } from '@/test/msw/server';
import { VerificationSection } from './VerificationSection';
import { verification } from './test/fixtures';
import { problem, verificationRead, verificationWrite } from './test/handlers';

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <VerificationSection projectId="project-a" />
    </QueryClientProvider>
  );
}

function controls() {
  const section = screen.getByTestId('verification');
  return {
    select: within(section).getByRole('combobox', { name: 'Heavy checks run in' }),
    save: within(section).getByRole('button', { name: /Save|Saving…/ })
  };
}

describe('verification section', () => {
  it('explains what automatic resolved to and what it found', async () => {
    server.use(verificationRead(() => HttpResponse.json(verification())));
    renderSection();
    expect(
      await screen.findByText(
        'Heavy checks run in: Hybrid (automatic, CI found on main: github-actions)'
      )
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        'Agents run the tests of what they changed, push, and let CI run the full suite.'
      )
    ).toBeInTheDocument();
    expect(controls().save).toBeDisabled();
  });

  it('saves the owner’s mode and shows the new setting', async () => {
    let body: unknown;
    server.use(
      verificationRead(() => HttpResponse.json(verification())),
      verificationWrite(async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(
          verification({ mode: 'ci', effective_mode: 'ci', source: 'owner' })
        );
      })
    );
    const user = userEvent.setup();
    renderSection();
    await screen.findByText(/Heavy checks run in: Hybrid/);
    await user.selectOptions(controls().select, 'ci');
    await user.click(controls().save);
    expect(await screen.findByText('Heavy checks run in: CI')).toBeInTheDocument();
    expect(body).toEqual({ mode: 'ci' });
  });

  it('says when CI was never detected, and when a re-check failed', async () => {
    server.use(
      verificationRead(() =>
        HttpResponse.json(
          verification({
            effective_mode: 'local',
            source: 'default',
            detection: null,
            detect_error: 'CI could not be read: status 502'
          })
        )
      )
    );
    renderSection();
    expect(
      await screen.findByText('Heavy checks run in: Workspace (CI not detected yet)')
    ).toBeInTheDocument();
    expect(
      screen.getByText("Could not re-check the project's CI: CI could not be read: status 502")
    ).toBeInTheDocument();
  });

  it('shows a load failure with a retry', async () => {
    server.use(verificationRead(problem(500, 'boom')));
    renderSection();
    expect(await screen.findByText('Could not load the checks setting.')).toBeInTheDocument();
  });
});
