import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { expect, test, vi } from 'vitest';
import tokens from '../../../design/tokens.json';
import { ConfirmDialog, type ConfirmDialogProps } from './confirm-dialog';

const props = (overrides: Partial<ConfirmDialogProps> = {}): ConfirmDialogProps => ({
  open: true,
  onOpenChange: vi.fn(),
  title: 'Cancel this task?',
  description: 'The running attempt is stopped. This cannot be undone.',
  confirmLabel: 'Cancel task',
  destructive: true,
  onConfirm: vi.fn(),
  ...overrides
});

/** Opens the dialog from a button, the way the pages do, so focus has somewhere to return to. */
function Opener() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open confirm
      </button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Cancel this task?"
        description="The running attempt is stopped."
        confirmLabel="Cancel task"
        destructive
        onConfirm={vi.fn()}
      />
    </>
  );
}

test('is a 28rem column (max-w-md from the max-width token), not a spacing-sized sliver', () => {
  render(<ConfirmDialog {...props()} />);
  expect(screen.getByRole('alertdialog')).toHaveClass('max-w-md');
  expect(tokens['max-width'].md.$value).toBe('28rem');
});

test('is an alert dialog named by its title, with Cancel focused first', async () => {
  render(<ConfirmDialog {...props()} />);
  expect(screen.getByRole('alertdialog', { name: 'Cancel this task?' })).toBeInTheDocument();
  expect(
    screen.getByText('The running attempt is stopped. This cannot be undone.')
  ).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus());
});

test('confirm runs the action; cancel closes without running it', async () => {
  const user = userEvent.setup();
  const p = props();
  render(<ConfirmDialog {...p} />);
  await user.click(screen.getByRole('button', { name: 'Cancel task' }));
  expect(p.onConfirm).toHaveBeenCalledTimes(1);
  await user.click(screen.getByRole('button', { name: 'Cancel' }));
  expect(p.onOpenChange).toHaveBeenCalledWith(false);
  expect(p.onConfirm).toHaveBeenCalledTimes(1);
});

test('Escape asks to close', async () => {
  const user = userEvent.setup();
  const p = props();
  render(<ConfirmDialog {...p} />);
  await user.keyboard('{Escape}');
  expect(p.onOpenChange).toHaveBeenCalledWith(false);
});

test('Escape closes the dialog and returns focus to the button that opened it', async () => {
  const user = userEvent.setup();
  render(<Opener />);
  const opener = screen.getByRole('button', { name: 'Open confirm' });
  await user.click(opener);
  await waitFor(() => expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus());
  await user.keyboard('{Escape}');
  await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
  await waitFor(() => expect(opener).toHaveFocus());
});

test('while pending both buttons are disabled and Escape cannot dismiss the dialog', async () => {
  const user = userEvent.setup();
  const p = props({ pending: true });
  render(<ConfirmDialog {...p} />);
  expect(screen.getByRole('button', { name: 'Cancel task' })).toBeDisabled();
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
  await user.keyboard('{Escape}');
  expect(p.onOpenChange).not.toHaveBeenCalled();
});
