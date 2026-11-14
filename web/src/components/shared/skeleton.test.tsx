import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SkeletonHeader, SkeletonRows } from './skeleton';

describe('skeleton placeholders', () => {
  it('draws the rows asked for, at the control height, hidden from assistive technology', () => {
    render(<SkeletonRows rows={3} testId="rows" />);
    const rows = screen.getByTestId('rows');
    expect(rows).toHaveAttribute('aria-hidden', 'true');
    expect(rows.children).toHaveLength(3);
    for (const row of rows.children) expect(row).toHaveClass('min-h-control');
  });

  it('varies the line widths, so a list does not read as a solid block', () => {
    render(<SkeletonRows rows={4} testId="rows" />);
    const widths = [...screen.getByTestId('rows').children].map((row) =>
      [...(row.lastElementChild?.classList ?? [])].find((name) => name.startsWith('w-'))
    );
    expect(new Set(widths).size).toBe(4);
  });

  it('draws a page header’s shape, hidden from assistive technology', () => {
    render(<SkeletonHeader testId="header" />);
    const header = screen.getByTestId('header');
    expect(header).toHaveAttribute('aria-hidden', 'true');
    expect(header.children).toHaveLength(4);
  });
});
