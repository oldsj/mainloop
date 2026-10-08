<script lang="ts">
  import {
    api,
    ApiError,
    type DevEnvironment,
    type EnvironmentVersion,
    type ProjectEnvironmentSelection
  } from '../api';
  let { projectId }: { projectId: string } = $props();
  let environments = $state<DevEnvironment[]>([]);
  let versions = $state<Record<string, EnvironmentVersion[]>>({});
  let saved = $state<ProjectEnvironmentSelection | null>(null);
  let loaded = $state(false);
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let environmentId = $state('');
  let versionId = $state('');
  let follow = $state(false);
  let name = $state('');
  let image = $state('');
  let architecture = $state<'amd64' | 'arm64'>('amd64');
  let registrationUncertain = $state(false);
  let generation = 0;
  const environment = $derived(environments.find((item) => item.id === environmentId));
  const candidates = $derived(versions[environmentId] ?? []);
  function eligible(version: EnvironmentVersion | undefined) {
    return (
      version?.validation_status === 'static_validated' &&
      version.validator_version === 'oci-static-v2'
    );
  }
  const target = $derived(
    candidates.find(
      (item) => item.id === (follow ? environment?.accepted_default_version_id : versionId)
    )
  );
  const canSave = $derived(loaded && !busy && !!environment && eligible(target));
  const dirty = $derived(
    environmentId !== saved?.environment_id ||
      follow !== (saved?.follow_default ?? false) ||
      (!follow && versionId !== saved?.version_id)
  );

  async function load(preserveDraft = false) {
    const token = ++generation;
    const id = projectId;
    loaded = false;
    try {
      const [rows, selection] = await Promise.all([
        api.listEnvironments(),
        api.getProjectEnvironment(id)
      ]);
      const metadata = await Promise.all(
        rows.map(async (row) => [row.id, await api.listEnvironmentVersions(row.id)] as const)
      );
      if (token !== generation || id !== projectId) return;
      environments = rows;
      versions = Object.fromEntries(metadata);
      saved = selection;
      loaded = true;
      if (!preserveDraft) {
        environmentId = selection?.environment_id ?? '';
        versionId = selection?.version_id ?? '';
        follow = selection?.follow_default ?? false;
      }
    } catch (cause) {
      if (token === generation && id === projectId) error = (cause as Error).message;
    }
  }
  $effect(() => {
    void projectId;
    void load();
    return () => {
      generation++;
    };
  });

  async function save() {
    if (!canSave || !dirty) return;
    const id = projectId;
    const token = generation;
    busy = true;
    error = '';
    notice = '';
    try {
      const selection = await api.selectProjectEnvironment(id, {
        environment_id: environmentId,
        version_id: follow ? null : versionId,
        follow_default: follow,
        expected_version: saved?.revision ?? 0
      } as import('../api').EnvironmentChoice);
      if (id === projectId && token === generation) {
        saved = selection;
        notice = 'Environment selection saved for new sessions.';
      }
    } catch (cause) {
      if (id !== projectId || token !== generation) return;
      error = `${(cause as Error).message}. Review the refreshed saved selection before saving again.`;
      await load(true);
    } finally {
      if (id === projectId) busy = false;
    }
  }
  async function acceptDefault() {
    const candidate = candidates.find((item) => item.id === versionId);
    if (!loaded || busy || !environment || !eligible(candidate)) return;
    const id = projectId;
    const token = generation;
    busy = true;
    error = '';
    notice = '';
    try {
      await api.acceptEnvironmentDefault(environmentId, versionId);
      if (id !== projectId || token !== generation) return;
      await load(true);
      notice =
        'Accepted default updated. Projects following this environment use it for new sessions.';
    } catch (cause) {
      if (id === projectId && token === generation) {
        error = (cause as Error).message;
        await load(true);
      }
    } finally {
      if (id === projectId) busy = false;
    }
  }
  async function register() {
    if (!loaded || busy || registrationUncertain) return;
    const id = projectId;
    const token = generation;
    busy = true;
    error = '';
    notice = '';
    try {
      const result = await api.registerEnvironment({
        name: name.trim(),
        image: image.trim(),
        architecture,
        source_kind: 'prebuilt_image'
      });
      if (id !== projectId || token !== generation) return;
      environments = [...environments, result.environment];
      versions = { ...versions, [result.environment.id]: [result.version] };
      notice = `Registered ${result.environment.name}: ${result.version.validation_status}, ${result.version.validator_version}. Runtime capabilities have not been tested.`;
      name = '';
      image = '';
    } catch (cause) {
      if (id !== projectId || token !== generation) return;
      registrationUncertain = !(cause instanceof ApiError) || cause.status >= 500;
      error = registrationUncertain
        ? 'Registration reply was not confirmed. Refresh the list and reconcile it before registering again.'
        : (cause as Error).message;
    } finally {
      if (id === projectId) busy = false;
    }
  }
</script>

<section class="border-term-border mb-6 border-b pb-6 text-sm" aria-label="Project environment">
  <h2 class="text-term-fg mb-3 font-semibold">Environment</h2>
  <p class="text-term-fg-muted mb-3">
    Selection applies to newly started sessions. Running sessions keep their recorded environment.
  </p>
  {#if loaded}
    <p class="text-term-fg mb-3 break-all">
      Saved: {#if saved}{environments.find((item) => item.id === saved?.environment_id)?.name ??
          saved.environment_id} · {saved.follow_default
          ? 'Follow accepted default'
          : 'Fixed version'} · {saved.resolved_version_id ??
          'No accepted default'}{#if saved.access_revoked}
          · Access revoked{/if}{:else}No selection. New sessions use the configured native agent
        image.{/if}
    </p>
    {#if !environments.length}<p class="text-term-fg-muted mb-3">
        No user-owned environments. Register a public image below to begin.
      </p>{/if}
    <fieldset disabled={busy} class="space-y-3">
      <legend class="text-term-fg-muted mb-2">Draft selection</legend>
      <label class="block"
        >Environment
        <select
          class="border-term-border bg-term-bg mt-1 block min-h-11 w-full border px-3 py-2"
          bind:value={environmentId}
          onchange={() => {
            versionId = '';
            follow = false;
            notice = '';
          }}
        >
          <option value="">Choose an environment</option>
          {#each environments as item (item.id)}<option value={item.id}>{item.name}</option>{/each}
        </select>
      </label>
      <label class="block"
        >Immutable version
        <select
          class="border-term-border bg-term-bg mt-1 block min-h-11 w-full border px-3 py-2"
          bind:value={versionId}
          disabled={!environment || follow}
        >
          <option value="">Choose a version</option>
          {#each candidates as item (item.id)}<option value={item.id} disabled={!eligible(item)}
              >{item.id} · {item.architecture ?? 'No platform'} · {eligible(item)
                ? 'Statically validated'
                : 'Unavailable: pending build or obsolete policy'}</option
            >{/each}
        </select>
      </label>
      <label class="flex min-h-11 items-center gap-2"
        ><input type="checkbox" bind:checked={follow} disabled={!environment} />Follow this
        environment’s accepted default</label
      >
      {#if follow}<p class="text-term-fg-muted break-all">
          Accepted default: {environment?.accepted_default_version_id ?? 'None'}. Future accepted
          defaults also apply to new sessions.
        </p>
        {#if !eligible(target)}<p class="text-term-yellow">
            An accepted current-policy version is required. Uncheck follow-default, choose a
            validated version, then accept it below.
          </p>{/if}
      {:else if environment && versionId && environment.accepted_default_version_id !== versionId}
        <button
          type="button"
          class="border-term-border min-h-11 border px-3 py-2 disabled:opacity-50"
          disabled={!eligible(candidates.find((item) => item.id === versionId))}
          onclick={acceptDefault}>Accept selected version as environment default</button
        >
        <p class="text-term-fg-muted">
          This changes the default for every project following this environment.
        </p>
      {/if}
      <button
        type="button"
        class="border-term-accent text-term-accent hover:bg-term-accent hover:text-term-bg min-h-11 border px-3 py-2 disabled:opacity-50"
        disabled={!canSave || !dirty}
        onclick={save}>Save selection</button
      >
    </fieldset>
  {:else}<p class="text-term-fg-muted">Loading saved selection and version metadata…</p>{/if}
  <button
    type="button"
    class="text-term-accent my-3 min-h-11 underline"
    disabled={busy}
    onclick={() => {
      error = '';
      void load(true);
    }}>Refresh saved selection and environments</button
  >
  <details>
    <summary class="text-term-fg min-h-11 cursor-pointer py-2">Register a public image</summary>
    <p class="text-term-fg-muted my-2">
      Use a public digest-pinned image from a supported registry. Static validation does not test
      runtime capabilities.
    </p>
    <form
      class="space-y-3"
      onsubmit={(event) => {
        event.preventDefault();
        void register();
      }}
    >
      <fieldset class="space-y-3" disabled={!loaded || busy || registrationUncertain}>
        <label class="block"
          >Name<input
            class="border-term-border bg-term-bg mt-1 block min-h-11 w-full border px-3 py-2"
            required
            maxlength="200"
            bind:value={name}
          /></label
        >
        <label class="block"
          >Image (registry/repository@sha256:digest)<input
            class="border-term-border bg-term-bg mt-1 block min-h-11 w-full border px-3 py-2"
            required
            pattern="[^\s]+/[^\s]+@sha256:[0-9a-f]{64}"
            bind:value={image}
            spellcheck="false"
          /></label
        >
        <label class="block"
          >Architecture<select
            class="border-term-border bg-term-bg mt-1 block min-h-11 border px-3 py-2"
            bind:value={architecture}
            ><option value="amd64">amd64</option><option value="arm64">arm64</option></select
          ></label
        >
        <button
          type="submit"
          class="border-term-border min-h-11 border px-3 py-2 disabled:opacity-50"
          >Register and validate image</button
        >
      </fieldset>
    </form>
  </details>
  {#if busy}<p class="text-term-fg-muted mt-3" role="status">Request in progress…</p>{/if}
  {#if error}<p class="text-term-red mt-3" role="alert">{error}</p>{/if}
  {#if notice}<p class="text-term-green mt-3 break-all" role="status">{notice}</p>{/if}
</section>
