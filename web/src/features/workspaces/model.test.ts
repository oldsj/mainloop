import { describe, expect, it } from 'vitest';
import { workspaceFixture } from '@/test/fixtures';
import {
  canResume,
  canSuspend,
  declaredPorts,
  digestPrefix,
  isHttpUrl,
  isWorkspace,
  lastActivity,
  lifecycleNotice,
  loadMessage,
  previewLinks,
  stateHint,
  UNCONFIRMED
} from './model';

describe('digestPrefix', () => {
  it('shows the first 19 characters of a well-formed digest', () => {
    const image = `registry.example/mainloop-dev@sha256:${'0123456789abcdef'.repeat(4)}`;
    expect(digestPrefix(image)).toBe('sha256:0123456789ab');
  });

  it('returns null when the image has no digest', () => {
    expect(digestPrefix('registry.example/mainloop-dev:latest')).toBeNull();
  });

  it('returns null for a malformed digest rather than a broken value', () => {
    expect(digestPrefix('registry.example/x@not-a-digest')).toBeNull();
    expect(digestPrefix('registry.example/x@sha256:XYZ')).toBeNull();
    expect(digestPrefix('registry.example/x@')).toBeNull();
    expect(digestPrefix('@@@')).toBeNull();
  });
});

describe('isHttpUrl', () => {
  it('accepts http and https only', () => {
    expect(isHttpUrl('https://example.test/')).toBe(true);
    expect(isHttpUrl('http://localhost:3000/')).toBe(true);
    expect(isHttpUrl('javascript:alert(1)')).toBe(false);
    expect(isHttpUrl('git@example.test:repo.git')).toBe(false);
    expect(isHttpUrl('not a url')).toBe(false);
  });
});

describe('isWorkspace', () => {
  it('accepts a workspace in the shape the backend sends', () => {
    expect(isWorkspace(workspaceFixture())).toBe(true);
  });

  it('rejects a state the page does not know', () => {
    expect(isWorkspace(workspaceFixture({ observed_state: 'exploded' as never }))).toBe(false);
  });

  it('rejects a manifest with no dev block, and non-objects', () => {
    const { manifest, ...rest } = workspaceFixture();
    expect(isWorkspace({ ...rest, manifest: { ...manifest, dev: undefined } })).toBe(false);
    expect(isWorkspace(null)).toBe(false);
    expect(isWorkspace('workspace')).toBe(false);
  });
});

describe('declaredPorts', () => {
  it('keeps readable ports and counts the rest', () => {
    const result = declaredPorts([
      { name: 'web', number: 3000 },
      { name: 'bad', number: 'three' },
      null,
      { name: 'api', number: 4000 }
    ]);
    expect(result.ports.map((port) => port.name)).toEqual(['web', 'api']);
    expect(result.dropped).toBe(2);
  });
});

describe('previewLinks', () => {
  it('keeps http and https rows, and drops a javascript: row with a note', () => {
    const result = previewLinks([
      { port: 3000, name: 'web', url: 'https://web.example.test/' },
      { port: 4000, name: 'evil', url: 'javascript:alert(1)' },
      { port: 5000, name: 'local', url: 'http://localhost:5000/' },
      { port: 'x', name: 'bad', url: 'https://bad.example.test/' }
    ]);
    expect(result.links.map((link) => link.url)).toEqual([
      'https://web.example.test/',
      'http://localhost:5000/'
    ]);
    expect(result.dropped).toBe(2);
  });
});

describe('lifecycle rules', () => {
  it('offers suspend only from running, and resume only from suspended', () => {
    expect(canSuspend('running')).toBe(true);
    expect(canSuspend('suspended')).toBe(false);
    expect(canResume('suspended')).toBe(true);
    expect(canResume('running')).toBe(false);
    expect(canResume('failed')).toBe(false);
  });

  it('says so while a state change is in flight, and when kagent has not confirmed', () => {
    expect(stateHint('suspending')).toMatch(/Changing state/);
    expect(stateHint('resuming')).toMatch(/Changing state/);
    expect(stateHint('unknown')).toBe(UNCONFIRMED);
    expect(stateHint('running')).toBeNull();
  });

  it('turns an unconfirmed result from an action into a warning, never a success', () => {
    expect(lifecycleNotice(workspaceFixture({ observed_state: 'unknown' }))).toEqual({
      tone: 'warn',
      text: UNCONFIRMED
    });
    expect(lifecycleNotice(workspaceFixture({ observed_state: 'suspended' }))).toBeNull();
  });
});

describe('loadMessage', () => {
  it('says what went wrong for each load failure', () => {
    expect(loadMessage('not-found').title).toBe('Workspace not found');
    expect(loadMessage('server').message).toBe('Mainloop returned a server error.');
    expect(loadMessage('unreachable').message).toBe("Can't reach the Mainloop backend.");
    expect(loadMessage('other').title).toBe('Workspace could not be loaded');
  });
});

describe('lastActivity', () => {
  it('reads a missing or invalid time as a plain word', () => {
    expect(lastActivity(null)).toBe('Not recorded');
    expect(lastActivity('not a date')).toBe('Unknown');
  });
});
