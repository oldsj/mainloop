import { describe, expect, it } from 'vitest';
import { projection, view } from '@/test/fixtures/tasks';
import { publicationAsk } from './model';

const waiting = (facts: Parameters<typeof projection>[0]) =>
  view({ status: 'waiting', reason: 'publication' }, { projection: projection(facts) });

describe('what a task that waits on publication needs', () => {
  it('asks nothing of a task that is not waiting on publication', () => {
    expect(publicationAsk(view({ status: 'running' }))).toBeNull();
    expect(publicationAsk(view({ status: 'waiting', reason: 'ci' }))).toBeNull();
    expect(publicationAsk(view({ status: 'completed' }))).toBeNull();
  });

  it('needs the owner when the pull request is open, CI passed and no merge is proposed', () => {
    const ask = publicationAsk(waiting({ pr_state: 'open', ci_state: 'success', pr_number: 12 }));
    expect(ask).toMatchObject({ kind: 'ready', needsOwner: true, prNumber: 12 });
    expect(ask?.text).toMatch(/^PR #12 is open and CI passed\./);
  });

  it('asks nothing while CI runs or has failed, or a merge is under way', () => {
    // Given the states where the agent or the merge service still has the next move.
    expect(publicationAsk(waiting({ pr_state: 'open', ci_state: 'pending' }))).toBeNull();
    expect(publicationAsk(waiting({ pr_state: 'open', ci_state: 'failure' }))).toBeNull();
    expect(publicationAsk(waiting({ pr_state: 'open', ci_state: 'unknown' }))).toBeNull();
    expect(
      publicationAsk(
        waiting({ pr_state: 'open', ci_state: 'success', merge_proposal_id: 'proposal-1' })
      )
    ).toBeNull();
  });

  it('explains a merged, a closed and a missing pull request without calling them the owner’s turn', () => {
    const merged = publicationAsk(waiting({ pr_state: 'merged', pr_number: 7 }));
    expect(merged).toMatchObject({ kind: 'merged', needsOwner: false });
    expect(merged?.text).toMatch(/cancel it to close it/);

    const closed = publicationAsk(waiting({ pr_state: 'closed', pr_number: 7 }));
    expect(closed).toMatchObject({ kind: 'closed', needsOwner: false });
    expect(closed?.text).toMatch(/^PR #7 was closed without merging/);

    const none = publicationAsk(waiting({ pr_state: 'unknown', pr_number: null, pr_url: null }));
    expect(none).toMatchObject({ kind: 'none', needsOwner: false, prNumber: null, prUrl: null });
  });

  it('does not call a pull request missing when one is linked but not observed yet', () => {
    expect(publicationAsk(waiting({ pr_state: 'unknown', pr_number: 9 }))).toBeNull();
  });

  it('offers only an https link', () => {
    const facts = { pr_state: 'open', ci_state: 'success', pr_number: 3 } as const;
    expect(
      publicationAsk(waiting({ ...facts, pr_url: 'https://github.com/acme/w/pull/3' }))?.prUrl
    ).toBe('https://github.com/acme/w/pull/3');
    expect(publicationAsk(waiting({ ...facts, pr_url: 'javascript:alert(1)' }))?.prUrl).toBeNull();
  });
});
