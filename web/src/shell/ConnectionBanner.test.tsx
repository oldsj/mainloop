import { act, fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { createConnection } from '@/api/connection';
import { createSignIn } from '@/api/signin';
import { ConnectionBanner } from './ConnectionBanner';

describe('ConnectionBanner', () => {
  it('stays hidden while the backend answers', async () => {
    const connection = createConnection({ probe: async () => true });
    render(<ConnectionBanner connection={connection} />);
    await act(async () => {
      await connection.check();
    });
    expect(screen.queryByTestId('connection-banner')).not.toBeInTheDocument();
  });

  it('collapses repeated offline reports into one line', async () => {
    const connection = createConnection({ probe: async () => false });
    render(<ConnectionBanner connection={connection} />);
    act(() => {
      connection.reportFailure();
      connection.reportFailure();
      connection.reportFailure();
    });
    // A report asks the probe; the probe cannot reach the backend, so the banner shows.
    expect(await screen.findAllByTestId('connection-banner')).toHaveLength(1);
    expect(screen.getByTestId('connection-banner')).toHaveTextContent(/^Offline.*· reconnecting…$/);
    // The text is one truncated line, so a phone never stacks it into several rows.
    expect(screen.getByText(/reconnecting…$/)).toHaveClass('truncate');
  });

  it('asks the owner to sign in again once a reload did not restore the sign-in', () => {
    const connection = createConnection({ probe: async () => true });
    const reload = vi.fn();
    const signIn = createSignIn({ reload, storage: null });
    render(<ConnectionBanner connection={connection} signIn={signIn} />);
    expect(screen.queryByTestId('signin-banner')).not.toBeInTheDocument();
    act(() => signIn.expired());
    expect(screen.getByTestId('signin-banner')).toHaveTextContent('Signed out');
    fireEvent.click(screen.getByRole('button', { name: 'Sign in again' }));
    expect(reload).toHaveBeenCalledTimes(1);
  });
});
