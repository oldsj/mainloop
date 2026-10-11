import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import type { HITLMergeDetails } from '@/lib/contract';
import { mergeContext } from '@/test/fixtures/hitl';
import {
  isReviewable,
  prLinkFor,
  safeExternalHref,
  safeGithubHref,
  unavailableNotes
} from './evidence';
import { VerifiedMerge, type LoadMergeDetails } from './merge-review';
import { hitlView } from '@/test/fixtures/hitl';

const facts = mergeContext();
if (!facts.summary) throw new Error('fixture needs a summary');
const ready = facts as typeof facts & { summary: NonNullable<typeof facts.summary> };

function page(overrides: Partial<HITLMergeDetails> = {}): HITLMergeDetails {
  return {
    request_id: 'request',
    proposal_id: ready.proposal_id ?? '',
    section: 'files',
    summary_digest: ready.summary_digest ?? '',
    head: 'feature',
    head_sha: 'b'.repeat(40),
    base: 'main',
    base_sha: 'c'.repeat(40),
    captured_at: '2026-10-09T20:00:00Z',
    cursor: 0,
    next_cursor: null,
    truncated: false,
    items: [],
    ...overrides
  };
}

const draw = (load: LoadMergeDetails, over: Partial<typeof ready> = {}) =>
  render(<VerifiedMerge facts={{ ...ready, ...over }} conversation={null} load={load} />);

describe('detail paging', () => {
  it('loads a section when it is first opened, not before', async () => {
    const user = userEvent.setup();
    const load = vi
      .fn<LoadMergeDetails>()
      .mockResolvedValue(
        page({ items: [{ filename: 'src/a.ts', status: 'modified', additions: 3, deletions: 1 }] })
      );
    draw(load);
    expect(load).not.toHaveBeenCalled();
    await user.click(screen.getByText('Changed paths'));
    expect(await screen.findByText('src/a.ts')).toBeInTheDocument();
    expect(load).toHaveBeenCalledWith('proposal-1', 'files', 0);
    expect(load).toHaveBeenCalledTimes(1);
  });

  it('appends the next page on Load more and stops when there is no cursor', async () => {
    const user = userEvent.setup();
    const load = vi
      .fn<LoadMergeDetails>()
      .mockResolvedValueOnce(page({ items: [{ filename: 'one.ts' }], next_cursor: '50' }))
      .mockResolvedValueOnce(
        page({ cursor: 50, items: [{ filename: 'two.ts' }], next_cursor: null })
      );
    draw(load);
    await user.click(screen.getByText('Changed paths'));
    await screen.findByText('one.ts');
    await user.click(screen.getByRole('button', { name: 'Load more paths' }));
    await screen.findByText('two.ts');
    expect(screen.getByText('one.ts')).toBeInTheDocument();
    expect(load).toHaveBeenLastCalledWith('proposal-1', 'files', 50);
    expect(screen.queryByRole('button', { name: 'Load more paths' })).not.toBeInTheDocument();
  });

  it('ignores a cursor that does not move forward', async () => {
    const user = userEvent.setup();
    const load = vi
      .fn<LoadMergeDetails>()
      .mockResolvedValue(page({ items: [{ filename: 'one.ts' }], next_cursor: '0' }));
    draw(load);
    await user.click(screen.getByText('Changed paths'));
    await screen.findByText('one.ts');
    expect(screen.queryByRole('button', { name: 'Load more paths' })).not.toBeInTheDocument();
  });

  it('refuses a page for another summary and offers a retry', async () => {
    const user = userEvent.setup();
    const load = vi
      .fn<LoadMergeDetails>()
      .mockResolvedValueOnce(
        page({ summary_digest: 'f'.repeat(64), items: [{ filename: 'evil.ts' }] })
      )
      .mockResolvedValueOnce(page({ items: [{ filename: 'good.ts' }] }));
    draw(load);
    await user.click(screen.getByText('Changed paths'));
    expect(await screen.findByRole('alert')).toHaveTextContent('different proposal or summary');
    expect(screen.queryByText('evil.ts')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('good.ts')).toBeInTheDocument();
  });

  it('keeps already loaded items when a later page fails', async () => {
    const user = userEvent.setup();
    const load = vi
      .fn<LoadMergeDetails>()
      .mockResolvedValueOnce(page({ items: [{ filename: 'one.ts' }], next_cursor: '50' }))
      .mockRejectedValueOnce(new Error('Gateway timeout.'));
    draw(load);
    await user.click(screen.getByText('Changed paths'));
    await screen.findByText('one.ts');
    await user.click(screen.getByRole('button', { name: 'Load more paths' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Gateway timeout.');
    expect(screen.getByText('one.ts')).toBeInTheDocument();
  });

  it('shows the description and CI sections, with unsafe result links dropped', async () => {
    const user = userEvent.setup();
    const load = vi.fn<LoadMergeDetails>(async (_p, section) =>
      section === 'description'
        ? page({ section, description: 'Line one\n<b>bold</b>', truncated: true })
        : page({
            section,
            items: [
              {
                kind: 'check_run',
                name: 'build',
                conclusion: 'success',
                html_url: 'https://ci.example/1'
              },
              {
                kind: 'commit_status',
                context: 'lint',
                state: 'pending',
                target_url: 'javascript:alert(1)'
              }
            ]
          })
    );
    const { container } = draw(load);
    await user.click(screen.getByText('PR description from GitHub'));
    await screen.findByText(/Line one/);
    expect(container.querySelector('b')).toBeNull();
    expect(screen.getByText('The saved description is truncated.')).toBeInTheDocument();
    await user.click(screen.getByText('Full CI list'));
    await screen.findByText(/build/);
    const links = screen.getAllByRole('link', { name: 'Open result' });
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAttribute('href', 'https://ci.example/1');
    expect(links[0]).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('drops loaded pages when the same call carries a new digest', async () => {
    const user = userEvent.setup();
    const load = vi
      .fn<LoadMergeDetails>()
      .mockResolvedValue(page({ items: [{ filename: 'old.ts' }] }));
    const { rerender } = draw(load);
    await user.click(screen.getByText('Changed paths'));
    await screen.findByText('old.ts');
    rerender(
      <VerifiedMerge
        facts={{ ...ready, summary_digest: 'f'.repeat(64) }}
        conversation={null}
        load={load}
      />
    );
    expect(screen.queryByText('old.ts')).not.toBeInTheDocument();
    expect(load).toHaveBeenCalledTimes(1);
  });

  it('does not start a second read for a section that is already loading', async () => {
    const user = userEvent.setup();
    let release!: (p: HITLMergeDetails) => void;
    const load = vi.fn<LoadMergeDetails>(() => new Promise((resolve) => (release = resolve)));
    draw(load);
    const summary = screen.getByText('Changed paths');
    await user.click(summary);
    await user.click(summary);
    await user.click(summary);
    expect(load).toHaveBeenCalledTimes(1);
    release(page({ items: [{ filename: 'late.ts' }] }));
    await waitFor(() => expect(screen.getByText('late.ts')).toBeInTheDocument());
  });
});

describe('links and evidence helpers', () => {
  it('only https links leave the app', () => {
    draw(vi.fn(), { pr_url: 'javascript:alert(1)', compare_url: 'http://example.com/diff' });
    expect(screen.queryByRole('link', { name: 'Open PR' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Compare diff' })).not.toBeInTheDocument();
    expect(safeExternalHref('https://github.com/o/r/pull/1')).toBe('https://github.com/o/r/pull/1');
    expect(safeExternalHref('/relative')).toBeNull();
    expect(safeGithubHref('https://evil.example/o/r/pull/1')).toBeNull();
    expect(safeGithubHref('https://github.com.evil.example/o/r')).toBeNull();
    expect(safeExternalHref(42)).toBeNull();
  });

  it('opens external links in a new tab without leaking the opener', () => {
    draw(vi.fn());
    const link = screen.getByRole('link', { name: 'Open PR' });
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('only ready, current evidence with a digest can back an approval', () => {
    expect(isReviewable(null)).toBe(false);
    expect(isReviewable(mergeContext())).toBe(true);
    expect(isReviewable(mergeContext({ stale: true }))).toBe(false);
    expect(isReviewable(mergeContext({ availability: 'unavailable' }))).toBe(false);
    expect(isReviewable(mergeContext({ summary: null }))).toBe(false);
    expect(isReviewable(mergeContext({ summary_digest: null }))).toBe(false);
    expect(isReviewable(mergeContext({ pr_number: 8 }))).toBe(false);
    expect(isReviewable(mergeContext({ repository: 'other/repo' }))).toBe(false);
  });

  it('a PR link must name the repository and number it is shown with', () => {
    const url = 'https://github.com/owner/repo/pull/7';
    expect(prLinkFor(url, 'owner/repo', 7)).toBe(url);
    expect(prLinkFor(url, 'owner/repo', 8)).toBeNull();
    expect(prLinkFor(url, 'other/repo', 7)).toBeNull();
    expect(prLinkFor(url, null, 7)).toBeNull();
    expect(prLinkFor('https://github.com/owner/repo/pull/7/files', 'owner/repo', 7)).toBeNull();
  });

  it('server notes are read only from a single matching entry, and clipped', () => {
    const entry = {
      tool_id: 'a',
      freshness_reason: 'x'.repeat(900),
      summary: { unavailable_reasons: ['r1', 7] }
    };
    const one = hitlView('request', { merge: [entry] as never });
    const notes = unavailableNotes(one, 'a');
    expect(notes[0]).toHaveLength(500);
    expect(notes).toContain('r1');
    expect(unavailableNotes(hitlView('request', { merge: [entry, entry] as never }), 'a')).toEqual(
      []
    );
    expect(unavailableNotes(one, 'zzz')).toEqual([]);
  });
});
