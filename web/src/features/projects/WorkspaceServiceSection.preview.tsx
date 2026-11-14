import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useState } from 'react';
import { $api } from '@/api';
import { WorkspaceServiceSection } from './WorkspaceServiceSection';

const serviceKey = (projectId: string) =>
  $api.queryOptions('get', '/projects/{project_id}/workspace-service', {
    params: { path: { project_id: projectId } }
  }).queryKey;

/**
 * One project with a declared service and one with none, seeded into the cache under the keys the
 * section reads. The stale time is infinite, so the preview makes no request.
 */
function seededClient() {
  const client = new QueryClient({
    defaultOptions: { queries: { staleTime: Infinity, retry: false } }
  });
  client.setQueryData(serviceKey('declared'), {
    command: ['scripts/dev/workspace.sh', 'run'],
    ports: [{ name: 'web', number: 3000 }]
  });
  client.setQueryData(serviceKey('none'), { command: [], ports: [] });
  return client;
}

export default function WorkspaceServiceSectionPreview() {
  const [client] = useState(seededClient);
  return (
    <QueryClientProvider client={client}>
      <div className="gap-lg p-md grid">
        <WorkspaceServiceSection projectId="declared" />
        <WorkspaceServiceSection projectId="none" />
      </div>
    </QueryClientProvider>
  );
}
