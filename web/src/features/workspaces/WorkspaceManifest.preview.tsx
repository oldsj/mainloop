import { workspaceFixture } from '@/test/fixtures';
import { WorkspaceManifest } from './WorkspaceManifest';

/** A declared manifest, and one with no development environment and a repository URL that is not a web link. */
export default function WorkspaceManifestPreview() {
  const declared = workspaceFixture({
    manifest: {
      repo_url: 'https://example.test/mainloop.git',
      ref: 'main',
      branch: 'feature/retry',
      depth: 1,
      agent_kind: 'codex',
      dev: { ports: [{ name: 'web', number: 3000 }], idle_timeout_minutes: 30 },
      development_environment: {
        environment_id: 'env-1',
        version_id: 'version-1',
        image: 'registry.example.test/mainloop/dev@sha256:0123456789abcdef0123',
        platform: 'linux/amd64',
        policy_identity: 'policy-1'
      }
    },
    last_activity_at: '2026-10-10T09:00:00Z'
  });
  const bare = workspaceFixture({
    workspace_id: 'w2',
    manifest: {
      repo_url: 'git@example.test:mainloop.git',
      ref: '',
      branch: 'main',
      depth: 0,
      agent_kind: 'claude',
      dev: { ports: [], idle_timeout_minutes: 15 },
      development_environment: null
    }
  });

  return (
    <div className="gap-lg p-md grid">
      <WorkspaceManifest workspace={declared} />
      <WorkspaceManifest workspace={bare} />
    </div>
  );
}
