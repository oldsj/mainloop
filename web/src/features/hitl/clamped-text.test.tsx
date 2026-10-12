import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { CLAMP_CHARS, CLAMP_LINES, ClampedText } from './clamped-text';

describe('ClampedText', () => {
  it('shows short text whole, with no toggle and no scroller of its own', () => {
    const { container } = render(<ClampedText what="arguments" text={'a\nb'} />);
    const pre = container.querySelector('pre');
    expect(pre).toHaveTextContent('a b');
    expect(pre?.className).not.toMatch(/overflow-(auto|y-auto)|max-h/);
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('shows the start of long text and the rest on request', async () => {
    const user = userEvent.setup();
    const lines = Array.from({ length: CLAMP_LINES + 5 }, (_, i) => `line ${i}`);
    const { container } = render(<ClampedText what="arguments" text={lines.join('\n')} />);
    const pre = container.querySelector('pre');
    expect(pre).toHaveTextContent(`line ${CLAMP_LINES - 1}`);
    expect(pre).not.toHaveTextContent(`line ${CLAMP_LINES}`);
    const toggle = screen.getByRole('button', { name: 'Show all of the arguments' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(pre).toHaveTextContent(`line ${CLAMP_LINES + 4}`);
    expect(screen.getByRole('button', { name: 'Show less of the arguments' })).toHaveAttribute(
      'aria-expanded',
      'true'
    );
  });

  it('clamps one very long line by characters', () => {
    const { container } = render(
      <ClampedText what="description" text={'x'.repeat(CLAMP_CHARS + 100)} />
    );
    expect(container.querySelector('pre')?.textContent).toHaveLength(CLAMP_CHARS + 1);
    expect(screen.getByRole('button', { name: 'Show all of the description' })).toBeInTheDocument();
  });
});
