import { mergeContext, mergeSummary } from '@/test/fixtures/hitl';
import { UnavailableMerge, VerifiedMerge } from './merge-review';

const never = () => Promise.reject(new Error('Not available in the preview.'));

const ready = mergeContext({
  summary: mergeSummary({
    approval_reasons: [
      { type: 'project_policy' },
      { type: 'protected_path', glob: 'k8s/**', path: 'k8s/protected.yaml' }
    ],
    ci: {
      complete: true,
      captured_at: '2026-10-09T20:00:00Z',
      result_count: 12,
      passed_count: 11,
      failed_count: 0,
      pending_count: 1,
      green_at_preparation: false,
      inventory_digest: 'f'.repeat(64)
    }
  }),
  deadline: '2026-10-09T23:00:00Z'
});
const stale = mergeContext({
  stale: true,
  freshness_reason: 'The pull request head moved after this summary was captured.'
});

/** Merge evidence: ready, withheld because stale, and missing altogether. */
export default function MergeReviewPreview() {
  if (!ready.summary || !stale.summary) return null;
  return (
    <div className="gap-md p-md grid">
      <VerifiedMerge
        facts={{ ...ready, summary: ready.summary }}
        conversation={null}
        load={never}
      />
      <VerifiedMerge
        facts={{ ...stale, summary: stale.summary }}
        conversation={null}
        load={never}
      />
      <UnavailableMerge toolId="call-1" notes={['The summary was not saved for this call.']} />
    </div>
  );
}
