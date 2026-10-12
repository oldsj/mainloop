import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test, vi } from 'vitest';
import { ErrorRecovery } from './error-recovery';

test('announces the title and message as an alert', () => {
  render(<ErrorRecovery title="Could not load the task" message="The server did not answer." />);
  const alert = screen.getByRole('alert');
  expect(alert).toHaveTextContent('Could not load the task');
  expect(alert).toHaveTextContent('The server did not answer.');
});

test('shows no retry button when there is nothing to retry', () => {
  render(<ErrorRecovery title="Could not load" message="Gone." />);
  expect(screen.queryByRole('button')).not.toBeInTheDocument();
});

test('retry runs once; while pending it cannot be pressed again', async () => {
  const user = userEvent.setup();
  const onRetry = vi.fn();
  const { rerender } = render(
    <ErrorRecovery title="Could not load" message="Gone." onRetry={onRetry} />
  );
  await user.click(screen.getByRole('button', { name: 'Try again' }));
  expect(onRetry).toHaveBeenCalledTimes(1);

  rerender(<ErrorRecovery title="Could not load" message="Gone." onRetry={onRetry} pending />);
  expect(screen.getByRole('button', { name: 'Try again' })).toBeDisabled();
});

test('server detail is shown as text, never as markup', () => {
  const { container } = render(
    <ErrorRecovery title="Refused" message={'<img src="x" onerror="alert(1)">'} />
  );
  expect(container.querySelector('img')).toBeNull();
  expect(screen.getByRole('alert')).toHaveTextContent('<img src="x" onerror="alert(1)">');
});
