import { createFileRoute } from '@tanstack/react-router';
import { TranscriptSearchPage, transcriptSearch } from '@/features/transcripts';

// The query is the URL's, so a search survives a reload and Back returns to it.
export const Route = createFileRoute('/transcripts')({
  validateSearch: (search) => transcriptSearch.parse(search),
  head: () => ({ meta: [{ title: 'Search transcripts · Mainloop' }] }),
  component: Transcripts
});

function Transcripts() {
  const search = Route.useSearch();
  return <TranscriptSearchPage search={search} />;
}
