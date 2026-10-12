import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Button } from './button';

describe('Button disabled style', () => {
  it('draws a disabled primary as an outline with muted text, not as an accent bar', () => {
    render(<Button disabled>Save policy</Button>);
    const button = screen.getByRole('button', { name: 'Save policy' });
    expect(button).toBeDisabled();
    expect(button).toHaveClass(
      'disabled:border-border',
      'disabled:bg-transparent',
      'disabled:text-muted-foreground'
    );
    expect(button).not.toHaveClass('disabled:opacity-50');
  });

  it('keeps the opacity fade for the other variants', () => {
    render(
      <Button variant="destructive" disabled>
        Cancel task
      </Button>
    );
    expect(screen.getByRole('button', { name: 'Cancel task' })).toHaveClass('disabled:opacity-50');
  });
});
