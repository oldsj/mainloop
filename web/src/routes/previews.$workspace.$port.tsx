import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';
import { PreviewSignIn } from '@/features/workspaces/PreviewSignIn';

// A preview host sends a page load without a preview cookie here, with the path it asked for.
export const Route = createFileRoute('/previews/$workspace/$port')({
  validateSearch: (search) =>
    z.object({ path: z.string().optional().catch(undefined) }).parse(search),
  head: () => ({ meta: [{ title: 'Opening preview · Mainloop' }] }),
  component: Preview
});

function Preview() {
  const { workspace, port } = Route.useParams();
  const { path = '/' } = Route.useSearch();
  return (
    <PreviewSignIn
      key={`${workspace}:${port}`}
      workspaceId={workspace}
      port={Number(port)}
      path={path}
    />
  );
}
