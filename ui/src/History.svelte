<script lang="ts">
    // The runs of all copy jobs, newest first. The system's backups are not here: their history is
    // the Backup page's.
    import {onMount} from 'svelte';
    import {i18n, t} from 'occulite-ui/i18n';

    import RunDialog from './RunDialog.svelte';
    import {get} from './api.js';
    import {RUN_PILLS, duration, formatSize, runPillClass, type Run} from './model.js';

    let {tick}: {tick: number} = $props();

    let runs = $state<Run[] | undefined>();
    let error = $state('');
    let showing = $state<Run | null>(null);

    async function load() {
        try {
            runs = (await get<{runs: Run[]}>('api/v1/runs')).runs;
            error = '';
        } catch (e) {
            error = (e as Error).message;
        }
    }
    onMount(() => void load());
    // the page's own polling ticks: the list follows without a timer of its own
    $effect(() => {
        if (tick > 0) void load();
    });

    const when = (iso: string) => new Date(iso).toLocaleString(i18n.language === 'de' ? 'de-DE' : 'en-GB', {dateStyle: 'short', timeStyle: 'short'});
    const pill = (r: Run) => (r.result in RUN_PILLS ? r.result : 'failed');
</script>

<section class="sy-section" id="history">
    <div class="sy-section-head"><h2>{t('History')}</h2></div>
    <p class="sy-lead">{t("The runs of the copy jobs. The system's backups are listed on the Backup page.")}</p>
    {#if error}<p class="sy-error" role="alert">{error}</p>{/if}
    {#if runs && !runs.length}
        <div class="sy-empty" data-testid="history-empty"><p>{t('No copy job has run yet.')}</p></div>
    {:else if runs}
        <div class="sy-tablewrap">
            <table class="sy-table" data-testid="history-table">
                <thead><tr><th>{t('Time')}</th><th>{t('Job')}</th><th>{t('Started')}</th><th>{t('Result')}</th><th>{t('Duration')}</th><th>{t('Files')}</th><th>{t('Size')}</th></tr></thead>
                <tbody>
                    {#each runs as r (r.job + r.id)}
                        {@const p = pill(r)}
                        <tr data-testid="history-row" data-run={r.id}>
                            <td><button class="sy-linkbutton" onclick={() => (showing = r)} data-testid="history-open">{when(r.start)}</button></td>
                            <td class="sy-cell-name" title={r.job_name}>{r.job_name}{#if r.dry_run}{' '}<span class="sy-badge">{t('tried')}</span>{/if}</td>
                            <td>{r.trigger === 'page' ? t('here') : t('timer')}</td>
                            <td><span class="sy-pill {runPillClass(p)}" data-state={p}><span>{t(RUN_PILLS[p] ?? 'failed')}</span>{#each Object.values(RUN_PILLS) as label (label)}<span class="sy-pill-size" aria-hidden="true">{t(label)}</span>{/each}</span></td>
                            <td>{duration(r.start, r.end)}</td>
                            <td>{r.copied + r.replaced}{#if r.deleted}{' · −'}{r.deleted}{/if}</td>
                            <td>{formatSize(r.bytes)}</td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    {:else if !error}
        <p class="sy-muted">{t('Loading…')}</p>
    {/if}
</section>

{#if showing}
    <RunDialog job={showing.job} run={showing.id} onclose={() => (showing = null)} />
{/if}
