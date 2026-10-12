import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ShortId } from './ShortId';

const FULL = '6f1c2a9e-4b7d-4e8a-9c3f-0d2b1a7e5f10';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('ShortId', () => {
  it('shows the first 8 characters, and the full ID only as a tooltip', () => {
    render(<ShortId id={FULL} label="workspace" />);
    const short = screen.getByText('6f1c2a9e');
    expect(short).toHaveAttribute('title', FULL);
    expect(screen.queryByText(FULL)).toBeNull();
  });

  it('copies the full ID from a labelled button and says so', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    render(<ShortId id={FULL} label="workspace" />);
    await userEvent.click(screen.getByRole('button', { name: 'Copy full workspace ID' }));
    expect(writeText).toHaveBeenCalledWith(FULL);
    expect(screen.getByRole('button', { name: 'Copy full workspace ID' })).toHaveTextContent(
      'Copied'
    );
  });

  it('says so when the clipboard refuses', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('denied'));
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    render(<ShortId id={FULL} label="workspace" />);
    await userEvent.click(screen.getByRole('button', { name: 'Copy full workspace ID' }));
    expect(await screen.findByRole('button', { name: 'Copy full workspace ID' })).toHaveTextContent(
      'Failed'
    );
  });
});
