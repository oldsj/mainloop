import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
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

test('while pending both buttons are disabled and Escape cannot dismiss the dialog', async () => {
  const user = userEvent.setup();
  const p = props({ pending: true });
  render(<ConfirmDialog {...p} />);
  expect(screen.getByRole('button', { name: 'Cancel task' })).toBeDisabled();
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
  await user.keyboard('{Escape}');
  expect(p.onOpenChange).not.toHaveBeenCalled();
});
