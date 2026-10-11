/**
 * Component tests ported from the frozen `frontend/src/lib/hitl.test.ts`:
 *   - "shared renderer: read-only states, escaped metadata, provider reasons and merge slots"
 *   - "renders observer/API-created requests from the isolated PostgreSQL integration"
 *   - "renders verified merge evidence from the integrated observer and owner API"
 *   - "merge display handles batches, stale evidence, and missing or untrusted facts"
 * and from `frontend/scripts/hitl-reactivity.mjs` (visiblePolling test 1, approval feedback).
 * The two integration tests read JSON files from the Python backend there; here the same states
 * are built in code, since the Go endpoints are not merged yet.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { buildHITLResponse, hitlStatus, reasonNotice, type HITLDraft } from '@/lib/hitl';
import type { HITLMergeDetails, HITLView, ToolApprovalRequest } from '@/lib/contract';
import {
  approvalRequest,
  decisionReceipt,
  hitlView,
  mergeContext,
  mergeSummary
} from '@/test/fixtures/hitl';
import { statusTone } from './evidence';
import { HITLRequest } from './hitl-request';
import { renderRouted, requestProps, snapshotOf } from './test-utils';

const show = (view: HITLView, extra: Parameters<typeof snapshotOf>[0] = {}) =>
  renderRouted(<HITLRequest {...requestProps({ snapshot: snapshotOf({ view, ...extra }) })} />);

const mergeTools = (ids = ['a', 'b']): ToolApprovalRequest => ({
  type: 'tool_approval_request',
  hint: '',
  nested: null,
  tools: ids.map((id) => ({
    id,
    call_id: id,
    name: 'unverified.merge_pull_request_with_approval',
    args: { merge: { ci: { green: true } } }
  }))
});

function withPayload(payload: unknown, overrides: Partial<HITLView> = {}): HITLView {
  const base = hitlView();
  return { ...base, ...overrides, request: { ...base.request, payload: payload as never } };
}

describe('shared renderer (frozen: shared renderer test)', () => {
  it('names the runtime in the reason notice and escapes agent-supplied args', () => {
    const view = hitlView('request', { context: { provider: 'codex' } });
    const { container } = show(view);
    expect(screen.getByText(/runtime receives rejection only/)).toBeInTheDocument();
    expect(container.querySelector('script')).toBeNull();
    expect(container).toHaveTextContent('<script>');
    expect(reasonNotice(null)).toMatch(/not verified/);
  });

  it('shows an uncertain receipt as read-only, with no way to send again', () => {
    const base = hitlView('request', { context: { provider: 'claude' } });
    const view = {
      ...base,
      response: decisionReceipt(base.request),
      transport_state: 'uncertain' as const
    };
    show(view);
    expect(screen.getByText(/Claude’s denial message/)).toBeInTheDocument();
    expect(screen.getByText('Keep tests')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('Decision delivery uncertain');
    expect(screen.queryByRole('button', { name: 'Send response' })).not.toBeInTheDocument();
  });

  it('shows a disabled state and disables every field when the server has writes off', () => {
    const { container } = show(hitlView('request', { writes_enabled: false }));
    expect(screen.getByRole('status')).toHaveTextContent('Responding disabled');
    expect(screen.getByText('Responding disabled by the server.')).toBeInTheDocument();
    expect(container.querySelectorAll('fieldset').length).toBeGreaterThan(0);
    for (const group of container.querySelectorAll('fieldset')) expect(group).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Send response' })).toBeDisabled();
  });

  it('refuses a malformed payload and offers no controls', () => {
    show(withPayload({ type: 'bad' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Malformed or unsupported');
    expect(screen.queryByRole('button', { name: 'Send response' })).not.toBeInTheDocument();
  });

  it('never links to a javascript: repository and shows no Merge action', () => {
    const view = hitlView('request', {
      merge: [{ tool_id: 'a', repository: 'javascript:alert(1)' }] as unknown as HITLView['merge']
    });
    const { container } = show(view);
    expect(screen.getByRole('region', { name: 'Merge context unavailable' })).toBeInTheDocument();
    expect(container.innerHTML).not.toMatch(/href="javascript/);
    expect(screen.queryByRole('button', { name: /^Merge$/ })).not.toBeInTheDocument();
    expect(
      hitlStatus({ ...hitlView(), request: { ...hitlView().request, availability: 'stale' } })
    ).toMatch(/stale/);
  });
});

describe('observer and API-created requests (frozen: isolated PostgreSQL integration test)', () => {
  const ordinary = approvalRequest({
    tools: [{ id: 'o', call_id: 'native-o', name: 'ordinary.tool', args: {} }]
  });

  it('pending, disabled, stale, unavailable and answered requests', () => {
    const states: [HITLView, RegExp][] = [
      [withPayload(ordinary), /Waiting for your response/],
      [withPayload(ordinary, { writes_enabled: false }), /Responding disabled/],
      [
        {
          ...withPayload(ordinary),
          request: { ...hitlView().request, payload: ordinary, availability: 'stale' }
        },
        /Observation stale/
      ],
      [withPayload(ordinary, { answerable: false }), /Request unavailable/],
      [
        (() => {
          const v = withPayload(ordinary);
          return {
            ...v,
            response: decisionReceipt(v.request),
            transport_state: 'accepted' as const
          };
        })(),
        /Decision delivered/
      ]
    ];
    for (const [view, status] of states) {
      const { unmount } = show(view);
      expect(screen.getByText('ordinary.tool')).toBeInTheDocument();
      expect(screen.getByRole('status')).toHaveTextContent(status);
      unmount();
    }
  });

  it('questions show every choice, a free-text field and escaped future metadata', () => {
    const questions = {
      type: 'ask_user_request',
      id: 'q',
      nested: null,
      questions: [
        { question: 'Which systems?', choices: ['Linux', 'macOS'], multiple: true },
        { question: 'Why?', choices: null, multiple: false }
      ],
      future_metadata: '<script>alert(1)</script>'
    };
    const { container } = show(withPayload(questions));
    expect(screen.getByRole('heading', { name: 'Questions for you' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Linux' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'macOS' })).toBeInTheDocument();
    expect(screen.getByLabelText('Your answer')).toBeInTheDocument();
    expect(container.querySelector('script')).toBeNull();
    expect(container).toHaveTextContent('future_metadata');
  });

  it('an answered question shows the recorded answers, matching what the form builds', () => {
    const payload = {
      type: 'ask_user_request',
      id: 'q',
      nested: null,
      questions: [
        { question: 'Which systems?', choices: ['Linux', 'macOS'], multiple: true },
        { question: 'Why?', choices: null, multiple: false }
      ]
    };
    const answer = buildHITLResponse(payload, {
      decisions: {},
      reasons: {},
      answers: [['Linux', 'macOS'], ['Keep tests']]
    });
    const base = withPayload(payload);
    show({
      ...base,
      response: { ...decisionReceipt(base.request), response: answer as never },
      transport_state: 'accepted'
    });
    expect(screen.getByText(/Answer 1: Linux, macOS/)).toBeInTheDocument();
    expect(screen.getByText(/Answer 2: Keep tests/)).toBeInTheDocument();
  });

  it('sends the chosen answers, one per question', async () => {
    const user = userEvent.setup();
    const onRespond = vi.fn<(draft: HITLDraft) => void>();
    const payload = {
      type: 'ask_user_request',
      id: 'q',
      nested: null,
      questions: [
        { question: 'Which systems?', choices: ['Linux', 'macOS'], multiple: true },
        { question: 'Why?', choices: null, multiple: false }
      ]
    };
    renderRouted(
      <HITLRequest
        {...requestProps({ snapshot: snapshotOf({ view: withPayload(payload) }), onRespond })}
      />
    );
    const send = screen.getByRole('button', { name: 'Send response' });
    expect(send).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Linux' }));
    await user.click(screen.getByRole('button', { name: 'macOS' }));
    expect(screen.getByRole('button', { name: 'Linux' })).toHaveAttribute('aria-pressed', 'true');
    await user.type(screen.getByLabelText('Your answer'), 'Keep tests');
    await user.click(send);
    expect(onRespond).toHaveBeenCalledWith(
      expect.objectContaining({ answers: [['Linux', 'macOS'], ['Keep tests']] })
    );
  });
});

describe('verified merge evidence (frozen: integrated observer and owner API test)', () => {
  it('shows the verified context with both SHAs and the protected path', () => {
    const facts = mergeContext({
      summary: mergeSummary({
        head_sha: 'a'.repeat(40),
        base_sha: 'b'.repeat(40),
        approval_reasons: [{ type: 'protected_path', glob: 'k8s/**', path: 'k8s/protected.yaml' }]
      }),
      head_sha: 'a'.repeat(40),
      base_sha: 'b'.repeat(40)
    });
    show(withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'] }));
    const region = screen.getByRole('region', { name: 'Verified merge context' });
    expect(region).toHaveTextContent('a'.repeat(40));
    expect(region).toHaveTextContent('b'.repeat(40));
    expect(region).toHaveTextContent('Protected path matched k8s/**: k8s/protected.yaml.');
    expect(screen.getByRole('button', { name: /^Approve/ })).toBeEnabled();
  });
});

describe('merge display batches (frozen: merge display test)', () => {
  it('shows unavailable evidence, never an approve, when two merge calls carry no facts', () => {
    show(withPayload(mergeTools(), { merge: null }));
    expect(screen.getAllByRole('region', { name: 'Merge context unavailable' })).toHaveLength(2);
    expect(screen.getAllByText(/Merge evidence unavailable · call/)).toHaveLength(2);
    expect(screen.queryByText(/Checks passed/)).not.toBeInTheDocument();
    for (const approve of screen.getAllByRole('button', { name: /^Approve/ }))
      expect(approve).toBeDisabled();
    for (const reject of screen.getAllByRole('button', { name: /^Reject/ }))
      expect(reject).toBeEnabled();
  });

  it('handles stale and unavailable evidence in one batch, with escaped text', () => {
    const a = mergeContext({
      tool_id: 'a',
      proposal_id: 'proposal-a',
      stale: true,
      state: 'evaluating',
      deadline: '2026-10-09T23:00:00Z',
      freshness_reason: 'The proposal is no longer current.',
      summary: mergeSummary({
        proposal_id: 'proposal-a',
        title: 'Update workspace defaults',
        approval_reasons: [{ type: 'protected_path', glob: 'k8s/**', path: 'k8s/<script>.yaml' }],
        ci: {
          complete: true,
          captured_at: '2026-10-09T20:00:00Z',
          result_count: 3,
          passed_count: 3,
          failed_count: 0,
          pending_count: 0,
          green_at_preparation: true,
          inventory_digest: 'f'.repeat(64)
        }
      })
    });
    const b = mergeContext({
      tool_id: 'b',
      proposal_id: 'proposal-b',
      pr_number: 18,
      availability: 'unavailable',
      freshness_reason: 'CI evidence unavailable',
      summary: mergeSummary({
        proposal_id: 'proposal-b',
        pr_number: 18,
        ci: null,
        availability: 'unavailable',
        unavailable_reasons: ['CI evidence unavailable']
      })
    });
    const { container } = show(withPayload(mergeTools(), { merge: [a, b] as HITLView['merge'] }));
    expect(screen.getByText('Update workspace defaults')).toBeInTheDocument();
    expect(screen.getAllByText('Approval required because:')).toHaveLength(2);
    expect(screen.getByText(/Protected path matched k8s\/\*\*/)).toBeInTheDocument();
    expect(screen.getByText(/owner\/repo #7/)).toBeInTheDocument();
    expect(screen.getByText(/owner\/repo #18/)).toBeInTheDocument();
    expect(screen.getByText(/The proposal is no longer current/)).toBeInTheDocument();
    expect(screen.getByText(/Evaluation deadline:/)).toBeInTheDocument();
    expect(screen.getByText(/3 results recorded at/)).toBeInTheDocument();
    expect(screen.getByText(/Recorded CI evidence is unavailable/)).toBeInTheDocument();
    expect(screen.getAllByText(/CI evidence unavailable/).length).toBeGreaterThan(0);
    const approves = screen.getAllByRole('button', { name: /^Approve/ });
    expect(approves).toHaveLength(2);
    for (const approve of approves) expect(approve).toBeDisabled();
    for (const reject of screen.getAllByRole('button', { name: /^Reject/ }))
      expect(reject).toBeEnabled();
    expect(container.querySelector('script')).toBeNull();
    expect(container).toHaveTextContent('k8s/<script>.yaml');
    expect(screen.queryByRole('button', { name: /^Merge$/ })).not.toBeInTheDocument();
  });

  it('treats duplicate entries for one call as ambiguous and shows both as unavailable', () => {
    const a = mergeContext({ tool_id: 'a' });
    show(withPayload(mergeTools(), { merge: [a, a] as HITLView['merge'] }));
    expect(screen.queryByText(/owner\/repo #7/)).not.toBeInTheDocument();
    expect(screen.getAllByRole('region', { name: 'Merge context unavailable' })).toHaveLength(2);
  });
});

describe('evidence that is missing at summary level', () => {
  it('summary null renders unavailable with Approve disabled and Reject available', () => {
    const facts = mergeContext({ tool_id: 'a', summary: null, availability: 'unavailable' });
    show(withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'] }));
    const region = screen.getByRole('region', { name: 'Merge context unavailable' });
    expect(region).toHaveTextContent('Merge evidence unavailable · call a');
    expect(screen.getByRole('button', { name: /^Approve/ })).toBeDisabled();
    expect(screen.getByRole('button', { name: /^Reject/ })).toBeEnabled();
  });

  it('says the page checks SHAs, digests and the repository more strictly than the API', () => {
    const facts = mergeContext({ tool_id: 'a', head_sha: 'not-a-sha' });
    show(withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'] }));
    const region = screen.getByRole('region', { name: 'Merge context unavailable' });
    expect(within(region).getByText(/stricter than the API schema/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Approve/ })).toBeDisabled();
  });

  it('shows the server note as text when the entry is unambiguous', () => {
    const entry = { tool_id: 'a', freshness_reason: '<b>Summary was withdrawn.</b>' };
    const { container } = show(
      withPayload(mergeTools(['a']), { merge: [entry] as unknown as HITLView['merge'] })
    );
    expect(container.querySelector('b')).toBeNull();
    expect(container).toHaveTextContent('Server note: <b>Summary was withdrawn.</b>');
  });

  it('a stale summary is shown but cannot be approved', async () => {
    const user = userEvent.setup();
    const onRespond = vi.fn();
    const facts = mergeContext({ tool_id: 'a', stale: true, freshness_reason: 'Head moved.' });
    renderRouted(
      <HITLRequest
        {...requestProps({
          snapshot: snapshotOf({
            view: withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'] })
          }),
          onRespond
        })}
      />
    );
    expect(screen.getByRole('region', { name: 'Verified merge context' })).toHaveTextContent(
      'Head moved. Approve is disabled; Reject stays available.'
    );
    expect(screen.getByRole('button', { name: /^Approve/ })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: /^Reject/ }));
    await user.click(screen.getByRole('button', { name: 'Send response' }));
    expect(onRespond).toHaveBeenCalledTimes(1);
    expect(onRespond.mock.calls[0]?.[0].reviewedContext).toEqual({});
  });

  it('an approval of ready evidence carries the reviewed summary digest', async () => {
    const user = userEvent.setup();
    const onRespond = vi.fn();
    const facts = mergeContext({ tool_id: 'a' });
    renderRouted(
      <HITLRequest
        {...requestProps({
          snapshot: snapshotOf({
            view: withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'] })
          }),
          onRespond
        })}
      />
    );
    await user.click(screen.getByRole('button', { name: /^Approve/ }));
    await user.click(screen.getByRole('button', { name: 'Send response' }));
    expect(onRespond.mock.calls[0]?.[0].reviewedContext).toEqual({ a: 'a'.repeat(64) });
  });
});

describe('approval feedback (frozen: hitl-reactivity visiblePolling test 1)', () => {
  it('tracks decisions and the rejection reason for ids that look like object keys', async () => {
    const user = userEvent.setup();
    const onRespond = vi.fn<(draft: HITLDraft) => void>();
    const payload = approvalRequest({
      tools: [
        { id: '__proto__', call_id: 'native-proto', name: 'tool.first', args: {} },
        { id: 'second', call_id: 'native-second', name: 'tool.second', args: {} }
      ]
    });
    renderRouted(
      <HITLRequest
        {...requestProps({ snapshot: snapshotOf({ view: withPayload(payload) }), onRespond })}
      />
    );
    const send = screen.getByRole('button', { name: 'Send response' });
    expect(send).toBeDisabled();

    const rejectFirst = screen.getByRole('button', { name: /^Reject tool\.first/ });
    await user.click(rejectFirst);
    expect(rejectFirst).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByLabelText(/Rejection reason/)).toBeInTheDocument();
    expect(send).toBeDisabled();

    await user.click(screen.getByRole('button', { name: /^Approve tool\.second/ }));
    expect(send).toBeEnabled();

    await user.type(screen.getByLabelText(/Rejection reason/), 'Keep tests');
    await user.click(send);
    const rejected = onRespond.mock.calls[0]![0];
    expect(buildHITLResponse(payload, rejected)).toMatchObject({
      approvals: [
        { id: '__proto__', approved: false, rejection_reason: 'Keep tests' },
        { id: 'second', approved: true }
      ]
    });

    await user.click(screen.getByRole('button', { name: /^Approve tool\.first/ }));
    expect(screen.queryByLabelText(/Rejection reason/)).not.toBeInTheDocument();
    await user.click(send);
    const approved = onRespond.mock.calls[1]![0];
    expect(Object.hasOwn(approved.reasons, '__proto__')).toBe(false);
    const response = buildHITLResponse(payload, approved);
    expect(response.type === 'tool_approval_response' && response.approvals[0]).toEqual({
      id: '__proto__',
      approved: true
    });
  });
});

describe('loading, errors and refresh', () => {
  it('shows a loading line before the first read', () => {
    renderRouted(<HITLRequest {...requestProps({ snapshot: snapshotOf({ view: null }) })} />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading session input…');
  });

  it('offers a refresh when the first read failed', async () => {
    const user = userEvent.setup();
    const onRefresh = vi.fn();
    renderRouted(
      <HITLRequest
        {...requestProps({
          snapshot: snapshotOf({ view: null, error: 'Could not refresh this request.' }),
          onRefresh
        })}
      />
    );
    await user.click(screen.getByRole('button', { name: 'Refresh status' }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it('locks every control while the decision is uncertain and says not to submit again', () => {
    const { container } = show(hitlView(), {
      uncertain: true,
      stale: true,
      error: 'Decision delivery uncertain. Checking the recorded result; do not submit again.'
    });
    expect(screen.getByRole('alert')).toHaveTextContent('do not submit again');
    expect(container.querySelectorAll('fieldset').length).toBeGreaterThan(0);
    for (const group of container.querySelectorAll('fieldset')) expect(group).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Send response' })).toBeDisabled();
  });

  it('shows Recording… and blocks a second press while a submit is in flight', () => {
    show(hitlView(), { busy: true });
    expect(screen.getByRole('button', { name: 'Recording…' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Refresh status' })).toBeDisabled();
  });

  it('links to the continuation request and the original conversation', () => {
    const view = hitlView('request', {
      route_request_id: 'next',
      context: {
        session_id: 's1',
        role: 'worker',
        title: 'Fix login',
        project_id: 'p1',
        project_name: 'Web'
      }
    });
    show(view);
    expect(screen.getByRole('link', { name: 'Open continuation request' })).toHaveAttribute(
      'href',
      '/?hitl=next'
    );
    expect(screen.getByRole('link', { name: 'Open original conversation' })).toHaveAttribute(
      'href',
      '/sessions/s1'
    );
    expect(screen.getByRole('link', { name: 'Web' })).toHaveAttribute('href', '/projects/p1');
    expect(screen.getByText(/Fix login/)).toBeInTheDocument();
  });
});

describe('follow-up 1: evidence identity', () => {
  const OLD = 'a'.repeat(64);
  const NEW = 'f'.repeat(64);
  const viewWith = (facts: ReturnType<typeof mergeContext>, over: Partial<HITLView> = {}) =>
    withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'], ...over });
  const detailsPage = (digest: string, filename: string): HITLMergeDetails => ({
    request_id: 'request',
    proposal_id: 'proposal-1',
    section: 'files',
    summary_digest: digest,
    head: 'feature',
    head_sha: 'b'.repeat(40),
    base: 'main',
    base_sha: 'c'.repeat(40),
    captured_at: '2026-10-09T20:00:00Z',
    cursor: 0,
    next_cursor: null,
    truncated: false,
    items: [{ filename }]
  });

  it('a new digest drops the loaded pages and clears the Approve made against the old one', async () => {
    const user = userEvent.setup();
    const onRespond = vi.fn();
    const load = vi.fn(async () => detailsPage(OLD, 'old-page.ts'));
    const props = (view: HITLView) =>
      requestProps({ snapshot: snapshotOf({ view }), onRespond, onLoadMergeDetails: load });
    const { rerender } = renderRouted(
      <HITLRequest {...props(viewWith(mergeContext({ summary_digest: OLD })))} />
    );
    await user.click(screen.getByText('Changed paths'));
    expect(await screen.findByText('old-page.ts')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /^Approve/ }));
    expect(screen.getByRole('button', { name: /^Approve/ })).toHaveAttribute(
      'aria-pressed',
      'true'
    );

    rerender(<HITLRequest {...props(viewWith(mergeContext({ summary_digest: NEW })))} />);
    expect(screen.queryByText('old-page.ts')).not.toBeInTheDocument();
    expect(screen.getByText('Expand to load captured paths.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Approve/ })).toHaveAttribute(
      'aria-pressed',
      'false'
    );
    expect(screen.getByRole('button', { name: 'Send response' })).toBeDisabled();
    expect(onRespond).not.toHaveBeenCalled();
  });

  it('a Reject survives a changed digest and never carries one', async () => {
    const user = userEvent.setup();
    const onRespond = vi.fn();
    const props = (view: HITLView) => requestProps({ snapshot: snapshotOf({ view }), onRespond });
    const { rerender } = renderRouted(
      <HITLRequest {...props(viewWith(mergeContext({ summary_digest: OLD })))} />
    );
    await user.click(screen.getByRole('button', { name: /^Reject/ }));
    rerender(<HITLRequest {...props(viewWith(mergeContext({ summary_digest: NEW })))} />);
    await user.click(screen.getByRole('button', { name: 'Send response' }));
    expect(onRespond.mock.calls[0]?.[0].reviewedContext).toEqual({});
  });

  it('shows the digested repository and PR, not the context copy, and treats a mismatch as unavailable', () => {
    const { unmount } = show(viewWith(mergeContext()));
    expect(screen.getByRole('region', { name: 'Verified merge context' })).toHaveTextContent(
      'owner/repo #7'
    );
    unmount();
    show(viewWith(mergeContext({ pr_number: 8 })));
    expect(screen.queryByRole('region', { name: 'Verified merge context' })).toBeNull();
    expect(screen.getByRole('region', { name: 'Merge context unavailable' })).toHaveTextContent(
      'different pull requests'
    );
    expect(screen.getByRole('button', { name: /^Approve/ })).toBeDisabled();
  });

  it('drops a PR link that does not point at the summary repository and number', () => {
    const { unmount } = show(viewWith(mergeContext()));
    expect(screen.getByRole('link', { name: 'Open PR' })).toHaveAttribute(
      'href',
      'https://github.com/owner/repo/pull/7'
    );
    unmount();
    show(viewWith(mergeContext({ pr_url: 'https://github.com/other/repo/pull/7' })));
    expect(screen.queryByRole('link', { name: 'Open PR' })).toBeNull();
  });
});

describe('follow-up 1: copy, links and guards', () => {
  it('names the merge-nav link differently from the header link', () => {
    const facts = mergeContext();
    const view = withPayload(mergeTools(['a']), {
      merge: [facts] as HITLView['merge'],
      context: { session_id: 's1', role: 'worker' }
    });
    show(view);
    expect(screen.getByRole('link', { name: 'Open original conversation' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open agent conversation' })).toBeInTheDocument();
  });

  it.each([null, 'something-new'])(
    'a receipt with transport state %s is worded and toned as recorded, not bad',
    (state) => {
      const base = hitlView();
      const view = { ...base, response: decisionReceipt(base.request), transport_state: state };
      show(view);
      expect(screen.getByRole('status')).toHaveTextContent('Decision recorded');
      expect(statusTone(view)).toBe('active');
    }
  );

  it.each(['sending', 'uncertain', 'rejected_transport'])(
    'a receipt in transport state %s keeps the bad tone',
    (state) => {
      const base = hitlView();
      expect(
        statusTone({ ...base, response: decisionReceipt(base.request), transport_state: state })
      ).toBe('bad');
    }
  );

  it('does not repeat the leading unavailable reason', () => {
    const facts = mergeContext({
      stale: true,
      summary: mergeSummary({ unavailable_reasons: ['Checks are missing.', 'Paths are missing.'] })
    });
    show(withPayload(mergeTools(['a']), { merge: [facts] as HITLView['merge'] }));
    expect(screen.getAllByText(/Checks are missing\./)).toHaveLength(1);
    expect(screen.getAllByText(/Paths are missing\./)).toHaveLength(1);
  });

  it('after a clean re-read with no decision, says what to do and keeps the lock', () => {
    const { container } = show(hitlView(), {
      uncertain: true,
      stale: false,
      error: 'Decision delivery uncertain. Checking the recorded result; do not submit again.'
    });
    expect(screen.getByRole('alert')).toHaveTextContent(
      'No decision was recorded. Reload to answer again.'
    );
    for (const group of container.querySelectorAll('fieldset')) expect(group).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Send response' })).toBeDisabled();
  });

  it.each([
    ['outer', (v: HITLView) => ({ ...v, request: { ...v.request, outer: undefined } })],
    ['payload', (v: HITLView) => ({ ...v, request: { ...v.request, payload: undefined } })],
    ['request', (v: HITLView) => ({ ...v, request: undefined })]
  ])('a view missing %s renders an error state instead of throwing', async (_name, break_) => {
    const user = userEvent.setup();
    const onRefresh = vi.fn();
    renderRouted(
      <HITLRequest
        {...requestProps({
          snapshot: snapshotOf({ view: break_(hitlView()) as unknown as HITLView }),
          onRefresh
        })}
      />
    );
    expect(screen.getByRole('alert')).toHaveTextContent('cannot display');
    expect(screen.queryByRole('button', { name: 'Send response' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Refresh status' }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it('lets a long ask-user choice wrap instead of scrolling the card sideways', async () => {
    const longChoice = 'Use the existing retry helper and keep the timeout. '.repeat(40);
    const view = withPayload({
      type: 'ask_user_request',
      id: 'q',
      nested: null,
      questions: [{ question: 'How?', choices: [longChoice.trim(), 'Short'], multiple: false }]
    });
    show(view);
    const button = screen.getByRole('button', { name: longChoice.trim() });
    // jsdom has no layout, so assert the classes that allow wrapping at phone width.
    expect(button).toHaveClass('whitespace-normal', 'h-auto', 'max-w-full', 'text-left');
    expect(button.className).not.toMatch(/(^|\s)whitespace-nowrap(\s|$)/);
    await waitFor(() => expect(button).toBeEnabled());
  });
});
