/**
 * Public surface of the HITL feature. The prop shapes are final; F3 builds the components.
 * Until then the components throw on render, so a caller that uses them early fails loudly.
 */
import type { HITLDraft, HITLMergeDetailSection, HITLMergeDetails, HITLState } from '@/lib/hitl';

export interface HITLCardProps {
  /** The approval or question request to show. The card reads its owning task from the cache. */
  requestId: string;
}

export interface HITLRequestProps {
  snapshot: HITLState;
  onRespond: (draft: HITLDraft) => void;
  onRefresh: () => void;
  onLoadMergeDetails: (
    proposalId: string,
    section: HITLMergeDetailSection,
    cursor: number
  ) => Promise<HITLMergeDetails>;
}

export const HITLCard: (props: HITLCardProps) => never = () => {
  throw new Error('HITLCard is built in F3');
};

export const HITLRequest: (props: HITLRequestProps) => never = () => {
  throw new Error('HITLRequest is built in F3');
};
