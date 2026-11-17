<script lang="ts">
    // A copy job's form: what is copied (the folder picker over the system's own storage, with what
    // is never copied said and why), where to, how, and when - four simple schedules; everything
    // beyond them is the timer's on the system's Services page, which this dialog links and whose
    // edits it leaves alone.
    import {Dialog} from 'occulite-ui';
    import {t} from 'occulite-ui/i18n';

    import ConfirmDialog from './ConfirmDialog.svelte';
    import {ApiError, get, post, put} from './api.js';
    import {WEEKDAYS, jobReady, patternsOf, specOf, timerHref, type Folder, type Job, type JobSpec, type Listing, type State} from './model.js';

    let {job, view, onclose, ondone}: {job?: Job; view: State; onclose: () => void; ondone: (j: Job, created: boolean) => void} = $props();

    // svelte-ignore state_referenced_locally
    let spec = $state<JobSpec>(specOf(job));
    // svelte-ignore state_referenced_locally
    let excludes = $state((job?.excludes ?? []).join('\n'));
    // svelte-ignore state_referenced_locally
    let target = $state(job ? `${job.target.kind}:${job.target.id}` : '');
    let busy = $state(false);
    let error = $state('');
    let picking = $state(false);
    let listing = $state<Listing | undefined>();
    let pickError = $state('');
    let showNever = $state(false);
    let confirmCustom = $state<string[] | null>(null);

    // the timer as the system has it: a custom one was edited on the Services page
    // svelte-ignore state_referenced_locally
    const wasCustom = job?.schedule.kind === 'custom' || (job?.schedule.kind === 'manual' && !!job.schedule.calendar?.length);
    // svelte-ignore state_referenced_locally
    const customCalendar = job?.schedule.calendar ?? [];
    const ready = $derived(jobReady({...spec, target: {...spec.target, id: target.split(':')[1] ?? ''}}));
    const relative = (p: string) => (p === view.copy.root ? '/' : p.replace(view.copy.root + '/', ''));

    const WHY: Record<string, string> = {
        config: "the system's and the addons' settings, with keys and passwords: they leave the system in its backup, encrypted",
        temp: 'temporary files',
        backups: "backups of the system: they go through the Backup page",
        program: 'program files, not data',
        own: "this addon's own files",
        root: 'choose a folder inside',
    };

    async function browse(path: string) {
        pickError = '';
        try {
            listing = await get<Listing>('api/v1/sources?path=' + encodeURIComponent(path));
        } catch (e) {
            pickError = (e as Error).message;
        }
    }

    function openPicker() {
        picking = true;
        void browse(spec.source && !spec.read_addon ? spec.source : '');
    }

    function choose(f: Folder) {
        spec.source = f.path;
        spec.read_addon = f.state === 'addon' ? (f.addon ?? '') : '';
        picking = false;
    }

    function body(replace: boolean) {
        const [kind, id] = target.split(':');
        return {
            ...spec,
            name: spec.name.trim(),
            target: {kind: kind ?? '', id: id ?? '', folder: spec.target.folder.trim()},
            excludes: patternsOf(excludes),
            max_delete: Math.max(0, Math.floor(Number(spec.max_delete) || 0)),
            bwlimit_kbps: Math.max(0, Math.floor(Number(spec.bwlimit_kbps) || 0)),
            schedule: {kind: spec.schedule.kind, time: spec.schedule.time, weekday: spec.schedule.weekday},
            ...(replace ? {replace_custom: true} : {}),
        };
    }

    async function save(replace = false) {
        busy = true;
        error = '';
        confirmCustom = null;
        try {
            const r = job ? await put<Job>(`api/v1/jobs/${job.id}`, body(replace)) : await post<Job>('api/v1/jobs', body(false));
            ondone(r, !job);
        } catch (e) {
            const b = e instanceof ApiError ? (e.body as {error?: string; calendar?: string[]}) : undefined;
            if (b?.error === 'custom-timer') confirmCustom = b.calendar ?? [];
            else error = (e as Error).message;
        } finally {
            busy = false;
        }
    }
</script>

<Dialog open title={job ? t('Edit the copy job') : t('New copy job')} width="44rem" {onclose} testId="job-dialog">
    {#if picking}
        <div class="sy-picker" data-testid="picker">
            <div class="sy-picker-head">
                <button class="hmm-button" onclick={() => void browse(listing?.parent ?? '')} disabled={!listing?.parent} data-testid="picker-up">{t('Up')}</button>
                <span class="sy-mono sy-picker-path" data-testid="picker-path">{listing ? listing.path : ''}</span>
            </div>
            {#if pickError}<p class="sy-error" role="alert">{pickError}</p>{/if}
            <ul class="sy-picker-list">
                {#each listing?.folders ?? [] as f (f.path)}
                    <li class="sy-picker-row" data-testid="picker-folder" data-state={f.state} data-name={f.name}>
                        {#if f.open && f.state !== 'never'}
                            <button class="sy-linkbutton sy-picker-name" onclick={() => void browse(f.path)} title={t('Open')}>{f.name}/</button>
                        {:else}
                            <span class="sy-picker-name" class:sy-struck={f.state === 'never'}>{f.name}/</span>
                        {/if}
                        <span class="sy-picker-note">
                            {#if f.state === 'never'}{t('never copied')}: {t(WHY[f.why ?? ''] ?? 'never copied')}
                            {:else if f.state === 'addon'}{t('data of the addon {id}: a job reads it with that addon\'s rights', {id: f.addon ?? ''})}
                            {:else if f.state === 'unreadable'}{t('not readable by Sync')}{/if}
                        </span>
                        {#if f.state === 'ok' || f.state === 'addon'}
                            <button class="hmm-button" onclick={() => choose(f)} data-testid="picker-choose">{t('Choose')}</button>
                        {/if}
                    </li>
                {:else}
                    <li class="sy-muted">{t('No folders here.')}</li>
                {/each}
            </ul>
            <button class="sy-linkbutton" onclick={() => (showNever = !showNever)} data-testid="picker-never">{t('Never copied: …')}</button>
            {#if showNever}
                <ul class="sy-help sy-never" data-testid="picker-never-list">
                    <li><span class="sy-mono">{view.copy.root}/etc</span> – {t(WHY.config!)}</li>
                    <li>{t(WHY.backups!)} (<span class="sy-mono">{view.copy.fixed.join(' ')}</span>)</li>
                    <li><span class="sy-mono">tmp, var, lost+found</span> – {t(WHY.temp!)}</li>
                    <li>{t(WHY.program!)}</li>
                </ul>
            {/if}
        </div>
    {:else}
        <form class="sy-form" onsubmit={(e) => { e.preventDefault(); if (ready) void save(); }}>
            <label class="sy-label" for="sy-job-name">{t('Name')}</label>
            <input id="sy-job-name" type="text" class="hmm-input" bind:value={spec.name} maxlength="60" data-testid="job-name" />

            <span class="sy-label">{t('Source')}</span>
            <div class="sy-row">
                <span class="sy-mono sy-source" data-testid="job-source" title={spec.source}>{spec.source ? relative(spec.source) : t('no folder chosen')}</span>
                <button type="button" class="hmm-button" onclick={openPicker} data-testid="job-pick">{t('Choose a folder…')}</button>
            </div>
            {#if spec.read_addon}
                <p class="sy-notice sy-span" data-testid="job-readnote">{t('This folder is the data of the addon {id}. The job reads it with that addon\'s rights, which also open the settings that addon keeps for itself - passwords and tokens in them included.', {id: spec.read_addon})}</p>
            {/if}

            <label class="sy-label" for="sy-job-target">{t('Target')}</label>
            <select id="sy-job-target" class="hmm-select" bind:value={target} data-testid="job-target">
                <option value="" disabled>{t('Choose…')}</option>
                {#if view.accounts.length}<optgroup label={t('Accounts')}>{#each view.accounts as a (a.id)}<option value={`account:${a.id}`}>{a.name} · {a.detail}</option>{/each}</optgroup>{/if}
                {#if view.servers.length}<optgroup label={t('SSH servers')}>{#each view.servers as s (s.id)}<option value={`server:${s.id}`}>{s.name} · {s.user}@{s.host}</option>{/each}</optgroup>{/if}
            </select>
            <label class="sy-label" for="sy-job-folder">{t('Folder on the target')}</label>
            <input id="sy-job-folder" type="text" class="hmm-input" bind:value={spec.target.folder} placeholder="openccu-lite/export" autocomplete="off" spellcheck="false" data-testid="job-folder" />

            <label class="sy-label" for="sy-job-mode">{t('Mode')}</label>
            <select id="sy-job-mode" class="hmm-select" bind:value={spec.mode} data-testid="job-mode">
                <option value="copy">{t('Copy: new and changed files; nothing is deleted on the target')}</option>
                <option value="mirror">{t('Mirror: the target becomes the source')}</option>
            </select>
            {#if spec.mode === 'mirror'}
                <p class="sy-error sy-span" data-testid="job-mirrornote">{t('Mirror deletes on the target what is gone from the source. The job runs only after you have tried it and looked at what it would do.')}</p>
                <label class="sy-label" for="sy-job-maxdel">{t('At most deletions per run')}</label>
                <input id="sy-job-maxdel" type="number" class="hmm-input" min="0" max="1000000" bind:value={spec.max_delete} data-testid="job-maxdelete" />
                <p class="sy-help sy-span">{t('A run that would delete more stops and says so. 0 = no limit.')}</p>
            {/if}

            <label class="sy-label" for="sy-job-excludes">{t('Leave out')}</label>
            <textarea id="sy-job-excludes" class="hmm-input sy-mono" rows="3" bind:value={excludes} placeholder={'*.tmp\n/cache/**'} spellcheck="false" data-testid="job-excludes"></textarea>
            <p class="sy-help sy-span">{t('One pattern per line, with * and ?; a leading / means from the top of the source.')}</p>
            <label class="sy-check sy-span"><input type="checkbox" bind:checked={spec.name_filters} data-testid="job-namefilters" /> {t('Leave out files that look like keys and tokens')} <span class="sy-mono sy-muted">({view.copy.name_filters.join(' ')})</span></label>

            <label class="sy-label" for="sy-job-bw">{t('Rate limit')}</label>
            <input id="sy-job-bw" type="number" class="hmm-input" min="0" bind:value={spec.bwlimit_kbps} data-testid="job-bwlimit" />
            <p class="sy-help sy-span">{t('In KiB/s; 0 = no limit.')}</p>

            <label class="sy-label" for="sy-job-schedule">{t('Schedule')}</label>
            <div class="sy-row">
                <select id="sy-job-schedule" class="hmm-select" bind:value={spec.schedule.kind} data-testid="job-schedule-kind">
                    <option value="manual">{t('manual only')}</option>
                    <option value="hourly">{t('hourly')}</option>
                    <option value="daily">{t('daily at …')}</option>
                    <option value="weekly">{t('weekly on … at …')}</option>
                    {#if wasCustom}<option value="custom">{t('custom (edited on the Services page)')}</option>{/if}
                </select>
                {#if spec.schedule.kind === 'weekly'}
                    <select class="hmm-select" bind:value={spec.schedule.weekday} aria-label={t('Weekday')} data-testid="job-weekday">
                        {#each WEEKDAYS as d, i (d)}<option value={d}>{new Date(2024, 0, 1 + i).toLocaleDateString(undefined, {weekday: 'long'})}</option>{/each}
                    </select>
                {/if}
                {#if spec.schedule.kind === 'daily' || spec.schedule.kind === 'weekly'}
                    <input type="time" class="hmm-input" bind:value={spec.schedule.time} aria-label={t('Time')} data-testid="job-time" />
                {/if}
            </div>
            {#if spec.schedule.kind === 'custom' || (wasCustom && spec.schedule.kind === 'manual')}
                <p class="sy-help sy-span sy-mono" data-testid="job-calendar">{#each customCalendar as c (c)}OnCalendar={c}<br />{/each}</p>
            {/if}
            <p class="sy-help sy-span">
                {#if spec.schedule.kind === 'manual'}{t('The job runs only when you start it. Its timer stays on the system, switched off.')}{' '}{/if}
                {#if job}<a href={timerHref(job)} target="_top" data-testid="job-timer-advanced">{t('Advanced: edit the timer on the Services page')}</a>{:else}{t('After saving, the Services page edits the timer in full.')}{/if}
            </p>
        </form>
    {/if}
    {#if error}<p class="sy-error" role="alert" data-testid="job-error">{error}</p>{/if}
    {#snippet buttons()}
        {#if picking}
            <button class="hmm-button" onclick={() => (picking = false)} data-testid="picker-back">{t('Back')}</button>
            <button class="hmm-button sy-primary" onclick={() => listing && choose(listing.self)} disabled={!listing || (listing.self.state !== 'ok' && listing.self.state !== 'addon')} data-testid="picker-choose-this">{t('Choose this folder')}</button>
        {:else}
            <button class="hmm-button" onclick={onclose}>{t('Cancel')}</button>
            <button class="hmm-button sy-primary" onclick={() => void save()} disabled={busy || !ready} data-testid="job-save">{t('Save')}</button>
        {/if}
    {/snippet}
</Dialog>

<ConfirmDialog
    open={confirmCustom !== null}
    title={t('Replace the timer')}
    message={t('This replaces the timer edited on the Services page ({calendar}) with the schedule chosen here.', {calendar: (confirmCustom ?? []).map((c) => `OnCalendar=${c}`).join(', ')})}
    confirmLabel={t('Replace')}
    danger
    testId="job-custom-dialog"
    onconfirm={() => void save(true)}
    onclose={() => (confirmCustom = null)}
/>
