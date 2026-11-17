<script lang="ts">
    // The copy jobs: a folder of the system, copied to an account or an SSH server. A job runs in a
    // unit of its own on the system - by its timer, or from here - never in this page's daemon.
    import {i18n, t} from 'occulite-ui/i18n';
    import type {NoticesStore} from 'occulite-ui/notices';

    import ConfirmDialog from './ConfirmDialog.svelte';
    import JobDialog from './JobDialog.svelte';
    import RunDialog from './RunDialog.svelte';
    import {ApiError, del, post} from './api.js';
    import {RUN_PILLS, formatSize, logHref, runPillClass, scheduleWords, timerHref, type Job, type State} from './model.js';

    let {view, notices, onchange}: {view: State; notices: NoticesStore; onchange: () => void} = $props();

    let busy = $state('');
    let dialog = $state<{job?: Job} | null>(null);
    let deleting = $state<Job | null>(null);
    let showing = $state<{job: string; run: string; seen: boolean} | null>(null);

    const copy = $derived(view.copy);
    const hasTarget = $derived(view.accounts.length + view.servers.length > 0);
    const when = (iso?: string) => (iso ? new Date(iso).toLocaleString(i18n.language === 'de' ? 'de-DE' : 'en-GB', {dateStyle: 'short', timeStyle: 'short'}) : '');
    const pill = (j: Job) => (j.running ? (j.running.waiting ? 'waiting' : 'running') : j.last ? (j.last.result in RUN_PILLS ? j.last.result : 'failed') : 'never');
    const schedule = (j: Job) => {
        const w = scheduleWords(j.schedule);
        return t(w.key, w.values);
    };

    async function run(j: Job, dry: boolean) {
        busy = j.id + (dry ? 'dry' : 'run');
        try {
            await post(`api/v1/jobs/${j.id}/run`, {dry_run: dry});
            notices.push('info', dry ? t('Trying "{name}": nothing is copied.', {name: j.name}) : t('"{name}" was started.', {name: j.name}));
        } catch (e) {
            const code = e instanceof ApiError ? e.code : '';
            notices.push('error', code === 'dry-run-first' ? t('A mirror job deletes on its target: try it first and look at what it would do.') : (e as Error).message);
        } finally {
            busy = '';
            setTimeout(onchange, 600);
        }
    }

    async function stop(j: Job) {
        try {
            await post(`api/v1/jobs/${j.id}/stop`);
        } catch (e) {
            notices.push('error', (e as Error).message);
        }
        setTimeout(onchange, 600);
    }

    async function remove() {
        if (!deleting) return;
        const j = deleting;
        try {
            await del(`api/v1/jobs/${j.id}`);
            notices.push('info', t('The copy job "{name}" was deleted. Nothing on its target was touched.', {name: j.name}));
        } catch (e) {
            notices.push('error', (e as Error).message);
        } finally {
            deleting = null;
            onchange();
        }
    }
</script>

<section class="sy-section" id="jobs">
    <div class="sy-section-head">
        <h2>{t('Copy jobs')}</h2>
        <button class="hmm-button sy-primary" onclick={() => (dialog = {})} disabled={!copy.supported || !hasTarget} data-testid="job-add">{t('New copy job…')}</button>
    </div>
    <p class="sy-lead">
        {t('Copy folders of this system to an account or an SSH server. Backups of the system are set up on the Backup page.')}
        <a href="/backup#targets" target="_top">{t('Backup page')}</a>
    </p>
    {#if !copy.supported}
        <div class="sy-banner sy-banner-warn" role="status" data-testid="banner-timers">{t('This system does not offer timers to addons: copy jobs need openccu-lite 2.0.0-beta.7 or newer.')}</div>
    {:else if !hasTarget}
        <div class="sy-empty" data-testid="jobs-notarget"><p>{t('A copy job needs somewhere to copy to: connect an account or add an SSH server first.')}</p></div>
    {:else if !copy.jobs.length}
        <div class="sy-empty" data-testid="jobs-empty">
            <p>{t('No copy job yet.')}</p>
            <p class="sy-muted">{t('A copy job is no backup of the system: files that change while they are copied arrive as they were at that moment.')}</p>
        </div>
    {/if}
    {#if copy.jobs.length}
        <div class="sy-tablewrap">
            <table class="sy-table" data-testid="jobs-table">
                <thead><tr><th>{t('Name')}</th><th>{t('Source → target')}</th><th>{t('Schedule')}</th><th>{t('Next')}</th><th>{t('Last run')}</th><th></th></tr></thead>
                <tbody>
                    {#each copy.jobs as j (j.id)}
                        {@const p = pill(j)}
                        <tr data-testid="job" data-job={j.id}>
                            <td class="sy-cell-name" title={j.name}>{j.name}{#if j.mode === 'mirror'}{' '}<span class="sy-badge" title={t('Mirror: files that are gone from the source are deleted on the target.')}>{t('mirror')}</span>{/if}{#if j.encrypt}{' '}<span class="sy-badge" title={t('The copies on the target are encrypted.')}>{t('encrypted')}</span>{/if}{#if j.versions}{' '}<span class="sy-badge" title={t('What a run replaces or deletes is kept in the folder .versions on the target.')}>{t('versions')}</span>{/if}</td>
                            <td class="sy-cell-path" title={`${j.source} → ${j.target_name}: ${j.target.folder}`}>
                                <span class="sy-mono">{j.source.replace(copy.root + '/', '')}</span> → {#if j.target_gone}<span class="sy-error-inline">{t('target missing')}</span>{:else}{j.target_name}: <span class="sy-mono">{j.target.folder}</span>{/if}
                            </td>
                            <td class="sy-cell-schedule" data-testid="job-schedule">{schedule(j)}{#if j.needs_dry_run}<span class="sy-muted" title={t('A mirror job deletes on its target: try it first and look at what it would do.')}> · {t('try it first')}</span>{/if}</td>
                            <td class="sy-cell-next" data-testid="job-next">{j.timer.next ? when(j.timer.next) : '–'}</td>
                            <td class="sy-cell-last">
                                <span class="sy-pill {runPillClass(p)}" data-testid="job-pill" data-state={p}><span>{t(RUN_PILLS[p] ?? 'failed')}</span>{#each Object.values(RUN_PILLS) as label (label)}<span class="sy-pill-size" aria-hidden="true">{t(label)}</span>{/each}</span>
                                <span class="sy-cell-lastline" data-testid="job-last">
                                    {#if j.running}
                                        {#if j.running.waiting}{t('for another job to end')}{:else}{t('{files} files · {size}', {files: j.running.files, size: formatSize(j.running.bytes)})}{#if j.running.percent}{' · '}{j.running.percent} %{/if}{/if}
                                    {:else if j.last}
                                        <button class="sy-linkbutton" onclick={() => (showing = {job: j.id, run: j.last!.id, seen: true})} data-testid="job-result">{j.last.dry_run ? t('tried {when}', {when: when(j.last.start)}) : when(j.last.start)}</button>
                                    {/if}
                                </span>
                            </td>
                            <!-- two lines, always: what the job does now, and what is done to the job -->
                            <td class="sy-cell-actions">
                                <div class="sy-actions-main">
                                    {#if j.running}
                                        <button class="hmm-button sy-sized" onclick={() => void stop(j)} data-testid="job-stop"><span>{t('Stop')}</span><span class="sy-size" aria-hidden="true">{t('Run now')}</span></button>
                                    {:else}
                                        <button class="hmm-button sy-sized" onclick={() => void run(j, false)} disabled={busy !== '' || !!j.needs_dry_run || !!j.target_gone} data-testid="job-run"><span>{t('Run now')}</span><span class="sy-size" aria-hidden="true">{t('Stop')}</span></button>
                                    {/if}
                                    <button class="hmm-button" onclick={() => void run(j, true)} disabled={busy !== '' || !!j.running || !!j.target_gone} title={t('Copies nothing: lists what a run would copy and delete.')} data-testid="job-try">{t('Try it')}</button>
                                </div>
                                <div class="sy-actions-more">
                                    <button class="sy-linkbutton" onclick={() => (dialog = {job: j})} disabled={!!j.running} data-testid="job-edit">{t('Edit')}</button>
                                    <a href={logHref(j)} target="_top" title={t('The runs of this job in the system log')} data-testid="job-log">{t('Log')}</a>
                                    <button class="sy-linkbutton" onclick={() => (deleting = j)} disabled={!!j.running} data-testid="job-delete">{t('Delete')}</button>
                                </div>
                            </td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    {/if}
</section>

{#if dialog}
    <JobDialog job={dialog.job} {view} onclose={() => { dialog = null; onchange(); }} ondone={(j, created) => { dialog = null; notices.push('info', created ? t('The copy job "{name}" was made.', {name: j.name}) : t('The copy job "{name}" was saved.', {name: j.name})); onchange(); }} />
{/if}

{#if showing}
    <RunDialog job={showing.job} run={showing.run} seen={showing.seen} onclose={() => { showing = null; onchange(); }} />
{/if}

<ConfirmDialog
    open={deleting !== null}
    title={t('Delete the copy job')}
    message={t('Delete the copy job "{name}"? Its timer {unit} and its history go with it; nothing on its target is touched.', {name: deleting?.name ?? '', unit: deleting?.timer.unit ?? ''})}
    confirmLabel={t('Delete')}
    danger
    testId="job-delete-dialog"
    onconfirm={() => void remove()}
    onclose={() => (deleting = null)}
/>

{#if copy.jobs.length}
    <p class="sy-muted sy-foot">{t('Each job is a timer on the system:')} {#each copy.jobs as j, i (j.id)}{i ? ', ' : ''}<a href={timerHref(j)} target="_top" data-testid="job-timer-link">{j.timer.unit}</a>{/each}. {t('The Services page edits its schedule in full.')}</p>
{/if}
