import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { transpileModule, ModuleKind } from 'typescript';

const source = await readFile(
  new URL('./components/ProjectEnvironment.svelte', import.meta.url),
  'utf8'
);
const env = { id: 'env', name: 'Public image', accepted_default_version_id: 'v2' };
const version = {
  id: 'v2',
  environment_id: 'env',
  validation_status: 'static_validated',
  validator_version: 'oci-static-v2'
};
class ApiError extends Error {
  status: number;
  constructor(status: number) {
    super('Stale expected version');
    this.status = status;
  }
}
async function harness(overrides = {}) {
  const writes: unknown[] = [];
  const api = {
    listEnvironments: async () => [env],
    listEnvironmentVersions: async () => [version],
    getProjectEnvironment: async () => null,
    selectProjectEnvironment: async (_id: string, body: Record<string, unknown>) => {
      writes.push(body);
      return { ...body, revision: 1 };
    },
    registerEnvironment: async () => {
      throw new Error('Disconnected');
    },
    ...overrides
  };
  // Execute the actual component handlers. Derived expressions remain lazy, as in Svelte;
  // route changes and asynchronous replies are driven explicitly without a DOM/live backend.
  let script = source.match(/<script lang="ts">([\s\S]*?)<\/script>/)![1];
  script = script
    .replace(/  import [\s\S]*?;\n/g, '')
    .replace(
      /const (\w+) = \$derived\(([\s\S]*?)\);/g,
      "Object.defineProperty(globalThis, '$1', { get: () => ($2) });"
    );
  const context = vm.createContext({
    api,
    ApiError,
    $props: () => ({ projectId: 'p1' }),
    $state: (v: unknown) => v,
    $effect: () => {}
  });
  vm.runInContext(
    transpileModule(script, { compilerOptions: { module: ModuleKind.None } }).outputText,
    context
  );
  const run = (code: string) => vm.runInContext(code, context);
  return { run, writes };
}

test('first selection uses observed null revision and exact fixed/follow choices', async () => {
  const h = await harness();
  await h.run('load()');
  h.run("environmentId = 'env'; versionId = 'v2'");
  await h.run('save()');
  assert.deepEqual(JSON.parse(JSON.stringify(h.writes[0])), {
    environment_id: 'env',
    version_id: 'v2',
    follow_default: false,
    expected_version: 0
  });
  h.run('follow = true');
  await h.run('save()');
  assert.deepEqual(JSON.parse(JSON.stringify(h.writes[1])), {
    environment_id: 'env',
    version_id: null,
    follow_default: true,
    expected_version: 1
  });
});

test('conflict refreshes observed revision while retaining the owner draft; no retry', async () => {
  let reads = 0;
  const h = await harness({
    getProjectEnvironment: async () => ({
      environment_id: 'env',
      version_id: 'other',
      follow_default: false,
      revision: ++reads === 1 ? 3 : 4
    }),
    selectProjectEnvironment: async () => {
      throw new ApiError(409);
    }
  });
  await h.run('load()');
  h.run("versionId = 'v2'");
  await h.run('save()');
  assert.equal(h.run('saved.revision'), 4);
  assert.equal(h.run('versionId'), 'v2');
  assert.match(h.run('error'), /Review the refreshed/);
  assert.equal(reads, 2);
});

test('a late old-route response cannot enable or replace current route state', async () => {
  let finish!: (value: unknown) => void;
  const h = await harness({
    getProjectEnvironment: () =>
      new Promise((resolve) => {
        finish = resolve;
      })
  });
  const pending = h.run('load()');
  h.run("projectId = 'p2'; generation++");
  finish(null);
  await pending;
  assert.equal(h.run('loaded'), false);
  assert.equal(h.run('environments.length'), 0);
});

test('pending and obsolete policy versions cannot be saved, including defaults', async () => {
  for (const rejected of [
    { ...version, validation_status: 'pending_build' },
    { ...version, validator_version: 'oci-static-v1' }
  ]) {
    const h = await harness({ listEnvironmentVersions: async () => [rejected] });
    await h.run('load()');
    h.run("environmentId = 'env'; versionId = 'v2'");
    assert.equal(h.run('canSave'), false);
    await h.run('save()');
    h.run('follow = true');
    await h.run('save()');
    assert.equal(h.writes.length, 0);
  }
});

test('uncertain registration blocks repeat POST even after a read refresh', async () => {
  let posts = 0;
  const h = await harness({
    registerEnvironment: async () => {
      posts++;
      throw new Error('Disconnected');
    }
  });
  await h.run('load()');
  await h.run('register()');
  await h.run('load(true)');
  await h.run('register()');
  assert.equal(posts, 1);
  assert.equal(h.run('registrationUncertain'), true);
});

test('owner API encodes paths and sends exact CAS/default/registration bodies; sanitized errors retain status', async () => {
  const apiSource = await readFile(new URL('./api.ts', import.meta.url), 'utf8');
  const requests: { url: string; init: RequestInit }[] = [];
  let fail = false;
  const context = vm.createContext({
    API_URL: 'http://offline',
    connection: { setConnected: () => {}, setDisconnected: () => {} },
    fetch: async (url: string, init: RequestInit) => {
      requests.push({ url, init });
      return new Response(
        JSON.stringify(fail ? { detail: 'Environment use grant required' } : null),
        { status: fail ? 403 : 200 }
      );
    }
  });
  const script = apiSource.replace(/import [\s\S]*?;/g, '').replace(/export /g, '');
  vm.runInContext(
    transpileModule(script, { compilerOptions: { module: ModuleKind.None } }).outputText,
    context
  );
  await vm.runInContext(
    "api.selectProjectEnvironment('project/a', {environment_id:'env', version_id:null, follow_default:true, expected_version:7})",
    context
  );
  assert.equal(requests[0].url, 'http://offline/projects/project%2Fa/environment');
  assert.equal(requests[0].init.method, 'PUT');
  assert.deepEqual(JSON.parse(requests[0].init.body as string), {
    environment_id: 'env',
    version_id: null,
    follow_default: true,
    expected_version: 7
  });
  await vm.runInContext("api.acceptEnvironmentDefault('env/a', 'v2')", context);
  assert.equal(requests[1].url, 'http://offline/environments/env%2Fa/default');
  assert.deepEqual(JSON.parse(requests[1].init.body as string), { version_id: 'v2' });
  await vm.runInContext(
    "api.registerEnvironment({name:'Public',image:'ghcr.io/test@sha256:digest',architecture:'arm64',source_kind:'prebuilt_image'})",
    context
  );
  assert.equal(requests[2].init.method, 'POST');
  assert.equal(requests[2].url, 'http://offline/environments');
  assert.deepEqual(JSON.parse(requests[2].init.body as string), {
    name: 'Public',
    image: 'ghcr.io/test@sha256:digest',
    architecture: 'arm64',
    source_kind: 'prebuilt_image'
  });
  fail = true;
  await assert.rejects(
    vm.runInContext('api.listEnvironments()', context),
    (error: { status: number; message: string }) =>
      error.status === 403 && error.message === 'Environment use grant required'
  );
});

test('registration uses the real returned status without selecting or accepting a default', async () => {
  const h = await harness({
    registerEnvironment: async () => ({
      environment: { ...env, id: 'registered' },
      version: { ...version, id: 'registered-version', environment_id: 'registered' }
    })
  });
  await h.run('load()');
  await h.run('register()');
  assert.equal(h.run('versions.registered[0].id'), 'registered-version');
  assert.match(h.run('notice'), /static_validated, oci-static-v2/);
  assert.equal(h.run('environmentId'), '');
  assert.equal(h.writes.length, 0);
});

test('server error after registration also blocks uncertain repeat POST', async () => {
  let posts = 0;
  const h = await harness({
    registerEnvironment: async () => {
      posts++;
      throw new ApiError(500);
    }
  });
  await h.run('load()');
  await h.run('register()');
  await h.run('register()');
  assert.equal(posts, 1);
  assert.equal(h.run('registrationUncertain'), true);
});
