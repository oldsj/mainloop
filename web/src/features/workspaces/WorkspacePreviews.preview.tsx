import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useState } from 'react';
import { keys } from '@/api';
import { WorkspacePreviews } from './WorkspacePreviews';

/**
 * The port list is seeded into the cache under the key the page reads, and the stale time is
 * infinite, so the preview makes no request. The last row is not a web link and is left out.
 */
function seededClient() {
  const client = new QueryClient({
    defaultOptions: { queries: { staleTime: Infinity, retry: false } }
  });
  client.setQueryData(keys.workspacePorts('w1'), {
    ports: [
      { port: 3000, name: 'web', url: 'https://w1-web.preview.example.test' },
      { port: 8080, name: 'api', url: 'http://w1-api.preview.example.test' },
      { port: 9000, name: 'files', url: 'ftp://w1-files.preview.example.test' }
    ]
  });
  return client;
}

export default function WorkspacePreviewsPreview() {
  const [client] = useState(seededClient);
  return (
    <QueryClientProvider client={client}>
      <div className="p-md">
        <WorkspacePreviews workspaceId="w1" />
      </div>
    </QueryClientProvider>
  );
}
