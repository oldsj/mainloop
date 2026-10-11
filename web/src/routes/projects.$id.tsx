import { createFileRoute } from '@tanstack/react-router';
import { ProjectPage } from '@/features/projects';

export const Route = createFileRoute('/projects/$id')({
  component: Project
});

function Project() {
  const { id } = Route.useParams();
  // Keyed so that moving to another project remounts the page, and its drafts and notices do not carry over.
  return <ProjectPage key={id} id={id} />;
}
