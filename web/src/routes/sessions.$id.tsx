import { createFileRoute } from '@tanstack/react-router';
import { SessionPage } from '@/features/sessions';
import { sessionSearch } from '@/features/transcripts';

export const Route = createFileRoute('/sessions/$id')({
  // `transcript=1` opens the transcript, at `entry` when a search hit links here.
  validateSearch: (search) => sessionSearch.parse(search),
  head: () => ({ meta: [{ title: 'Session · Mainloop' }] }),
  component: Session
});

function Session() {
  const { id } = Route.useParams();
  const { transcript, entry } = Route.useSearch();
  // Keyed so that moving to another session remounts the page, and its notice and pending actions do not carry over.
  return <SessionPage key={id} id={id} transcript={transcript === 1} entry={entry} />;
}
