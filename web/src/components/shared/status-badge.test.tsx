import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import { StatusBadge } from './status-badge';

test('shows its label as text and takes the tone colour', () => {
  render(<StatusBadge label="NEEDS INPUT" tone="attention" />);
  const badge = screen.getByText('NEEDS INPUT');
  expect(badge).toHaveClass('text-term-yellow');
  expect(badge).toHaveClass('border-term-yellow/50');
});

test('a failure is worded, not only coloured', () => {
  render(<StatusBadge label="FAILED" tone="bad" />);
  expect(screen.getByText('FAILED')).toHaveClass('text-term-red');
});
