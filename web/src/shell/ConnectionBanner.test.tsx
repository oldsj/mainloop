import { act, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { createConnection } from '@/api/connection';
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

  it('collapses repeated offline reports into one line', () => {
    const connection = createConnection({ probe: async () => false });
    render(<ConnectionBanner connection={connection} />);
    act(() => {
      connection.reportFailure();
      connection.reportFailure();
      connection.reportFailure();
    });
    expect(screen.getAllByTestId('connection-banner')).toHaveLength(1);
    expect(screen.getByTestId('connection-banner')).toHaveTextContent(/^Offline.*· reconnecting…$/);
    // The text is one truncated line, so a phone never stacks it into several rows.
    expect(screen.getByText(/reconnecting…$/)).toHaveClass('truncate');
  });
});
