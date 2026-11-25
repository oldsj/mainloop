import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { connection } from '@/api';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import { server } from '@/test/msw/server';
import { DevicesSection } from './DevicesSection';

type Device = components['schemas']['Device'];

// The section makes its reads through the app's limiter, which the tests skip. See fastReads.ts.
vi.mock('@/api/middleware', async (importOriginal) => {
  const { withFastReads } = await import('@/features/sessions/test/fastReads');
  return withFastReads(await importOriginal());
});

afterEach(async () => {
  await connection.check();
});

const url = (path: string) => `${API_URL}${path}`;

const ACTIVE: Device = {
  id: 'dev_0123456789abcdefghijklmnop',
  name: 'Test iPhone',
  platform: 'ios',
  key_thumbprint: 'AAAAAAAAAAAAbbbbbbbbbbbbccccccccccccdddddd',
  created_at: '2026-01-02T10:00:00Z',
  last_seen_at: null,
  revoked_at: null,
  current: false,
  push: null,
  approval_key: null
};

const PUSHING: Device = {
  id: 'dev_pushpushpushpushpushpushpu',
  name: 'Work Test iPhone',
  platform: 'ios',
  key_thumbprint: 'PPPPPPPPPPPPqqqqqqqqqqqqrrrrrrrrrrrrssssss',
  created_at: '2026-01-02T11:00:00Z',
  last_seen_at: '2026-01-02T11:05:00Z',
  revoked_at: null,
  current: false,
  push: { environment: 'production', updated_at: '2026-01-02T11:01:00Z', invalid_at: null },
  approval_key: null
};

const REVOKED: Device = {
  id: 'dev_zyxwvutsrqponmlkjihgfedcba',
  name: 'Old Test iPhone',
  platform: 'ios',
  key_thumbprint: 'ZZZZZZZZZZZZyyyyyyyyyyyyxxxxxxxxxxxxwwwwww',
  created_at: '2026-01-01T10:00:00Z',
  last_seen_at: '2026-01-01T11:00:00Z',
  revoked_at: '2026-01-01T12:00:00Z',
  current: false,
  push: { environment: 'production', updated_at: '2026-01-01T11:00:00Z', invalid_at: null },
  approval_key: null
};

/** Serves the list from memory; a DELETE revokes the device there, as the server does. */
function serve(initial: Device[]) {
  let rows = initial.map((device) => ({ ...device }));
  const calls = { list: 0, revoked: [] as string[] };
  server.use(
    http.get(url('/devices'), () => {
      calls.list++;
      return HttpResponse.json(rows);
    }),
    http.delete(url('/devices/:device_id'), ({ params }) => {
      const id = String(params.device_id);
      calls.revoked.push(id);
      if (!rows.some((device) => device.id === id))
        return HttpResponse.json({ detail: 'device not found' }, { status: 404 });
      rows = rows.map((device) =>
        device.id === id && device.revoked_at === null
          ? { ...device, revoked_at: '2026-01-03T09:00:00Z' }
          : device
      );
      return new HttpResponse(null, { status: 204 });
    })
  );
  return calls;
}

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  render(
    <QueryClientProvider client={queryClient}>
      <DevicesSection />
    </QueryClientProvider>
  );
  return userEvent.setup();
}

const row = async (name: string) =>
  (await screen.findAllByTestId('device')).find(
    (item) => within(item).queryByText(name, { selector: 'p' }) !== null
  )!;

describe('devices', () => {
  it('shows an approval key and the entry that pins it for the executor', async () => {
    const signing: Device = {
      ...PUSHING,
      approval_key: {
        thumbprint: 'TTTTTTTTTTTTuuuuuuuuuuuuvvvvvvvvvvvvwwwwww',
        jwk: { kty: 'EC', crv: 'P-256', x: 'X'.repeat(43), y: 'Y'.repeat(43) }
      }
    };
    serve([ACTIVE, signing, { ...REVOKED, approval_key: signing.approval_key }]);
    const user = renderSection();

    const active = await row('Work Test iPhone');
    expect(within(active).getByText('TTTTTTTTTTTT…')).toBeInTheDocument();
    await user.click(within(active).getByText('Pin its approval key for the executor'));
    expect(
      within(active).getByText(
        `- {kid: dev_pushpushpushpushpushpushpu, kty: EC, crv: P-256, x: ${'X'.repeat(43)}, y: ${'Y'.repeat(43)}}`
      )
    ).toBeVisible();

    expect(within(await row('Test iPhone')).getByText(/· no approval key/)).toBeInTheDocument();
    expect(within(await row('Test iPhone')).queryByText(/Pin its approval key/)).toBeNull();
    expect(within(await row('Old Test iPhone')).queryByText(/Pin its approval key/)).toBeNull();
  });

  it('lists every device, revoked ones included, with their facts', async () => {
    serve([ACTIVE, REVOKED]);
    renderSection();
    expect(await screen.findAllByTestId('device')).toHaveLength(2);

    const active = await row('Test iPhone');
    expect(within(active).getByText('Active')).toBeInTheDocument();
    expect(within(active).getByText(/iOS · registered .* · last seen never/)).toBeInTheDocument();
    expect(within(active).getByText(/dev_0123456789abcdefghijklmnop/)).toBeInTheDocument();
    expect(within(active).getByText('AAAAAAAAAAAA…')).toBeInTheDocument();
    expect(within(active).getByRole('button', { name: 'Revoke Test iPhone' })).toBeEnabled();

    const revoked = await row('Old Test iPhone');
    expect(within(revoked).getByText('Revoked')).toBeInTheDocument();
    expect(within(revoked).getByText(/· revoked /)).toBeInTheDocument();
    expect(within(revoked).queryByText(/last seen never/)).toBeNull();
    expect(within(revoked).queryByRole('button')).toBeNull();
    expect(within(revoked).queryByText(/notifications/)).toBeNull();
  });

  it('says whether each active device gets notifications', async () => {
    const refused: Device = {
      ...PUSHING,
      id: 'dev_refusedrefusedrefusedref',
      name: 'Refused Test iPhone',
      key_thumbprint: 'RRRRRRRRRRRRssssssssssssttttttttttttuuuuuu',
      push: { ...PUSHING.push!, invalid_at: '2026-01-02T12:00:00Z' }
    };
    serve([ACTIVE, PUSHING, refused]);
    renderSection();
    expect(within(await row('Test iPhone')).getByText(/· notifications off$/)).toBeInTheDocument();
    expect(
      within(await row('Work Test iPhone')).getByText(/· notifications on$/)
    ).toBeInTheDocument();
    expect(
      within(await row('Refused Test iPhone')).getByText(
        /· notifications stopped: Apple refused its token$/
      )
    ).toBeInTheDocument();
  });

  it('revokes a device after confirmation, then reads the list again', async () => {
    const calls = serve([ACTIVE, REVOKED]);
    const user = renderSection();
    await user.click(await screen.findByRole('button', { name: 'Revoke Test iPhone' }));

    const dialog = await screen.findByRole('alertdialog', { name: 'Revoke Test iPhone?' });
    expect(calls.revoked).toEqual([]);
    const listsBefore = calls.list;
    await user.click(within(dialog).getByRole('button', { name: 'Revoke' }));

    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(calls.revoked).toEqual([ACTIVE.id]);
    expect(calls.list).toBeGreaterThan(listsBefore);
    const active = await row('Test iPhone');
    await waitFor(() => expect(within(active).getByText('Revoked')).toBeInTheDocument());
    expect(within(active).queryByRole('button')).toBeNull();
  });

  it('sends nothing when the confirmation is cancelled', async () => {
    const calls = serve([ACTIVE]);
    const user = renderSection();
    await user.click(await screen.findByRole('button', { name: 'Revoke Test iPhone' }));
    const dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull());
    expect(calls.revoked).toEqual([]);
    expect(within(await row('Test iPhone')).getByText('Active')).toBeInTheDocument();
  });

  it('says so on the row when the revoke fails', async () => {
    serve([ACTIVE]);
    server.use(
      http.delete(url('/devices/:device_id'), () =>
        HttpResponse.json({ detail: 'device not found' }, { status: 404 })
      )
    );
    const user = renderSection();
    await user.click(await screen.findByRole('button', { name: 'Revoke Test iPhone' }));
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Revoke' })
    );

    const active = await row('Test iPhone');
    expect(await within(active).findByRole('alert')).toHaveTextContent(
      'Not revoked: Mainloop has no such device.'
    );
  });

  it('shows the empty state when no device is registered', async () => {
    serve([]);
    renderSection();
    expect(
      await screen.findByText(
        'No devices registered. Sign in from the iPhone app’s Settings to register one.'
      )
    ).toBeInTheDocument();
  });

  it('offers a retry when the list cannot be read', async () => {
    server.use(
      http.get(url('/devices'), () => HttpResponse.json({ detail: 'boom' }, { status: 500 }))
    );
    renderSection();
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load the devices');
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });
});
