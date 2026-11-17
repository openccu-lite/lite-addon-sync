<script lang="ts">
    // One run: what it did, and its file list with a filter. Opening a dry run's result here is
    // "looking at it" - which is what lets a mirror job run for real.
    import {onMount} from 'svelte';
    import {Dialog} from 'occulite-ui';
    import {i18n, t} from 'occulite-ui/i18n';

    import {get} from './api.js';
    import {RUN_PILLS, duration, formatSize, runPillClass, type Run} from './model.js';

    let {job, run, seen = false, onclose}: {job: string; run: string; seen?: boolean; onclose: () => void} = $props();

    let rec = $state<Run | undefined>();
    let released = $state(false);
    let lines = $state<{kind: string; text: string}[]>([]);
    let more = $state(false);
    let filter = $state('all');
    let error = $state('');

    onMount(async () => {
        try {
            // svelte-ignore state_referenced_locally
            const r = await get<{run: Run; released: boolean}>(`api/v1/runs/${job}/${run}${seen ? '?seen=1' : ''}`);
            rec = r.run;
            released = r.released;
            // svelte-ignore state_referenced_locally
            const l = await get<{lines: {kind: string; text: string}[]; more: boolean}>(`api/v1/runs/${job}/${run}/log`);
            lines = l.lines;
            more = l.more;
        } catch (e) {
            error = (e as Error).message;
        }
    });

    const when = (iso: string) => new Date(iso).toLocaleString(i18n.language === 'de' ? 'de-DE' : 'en-GB', {dateStyle: 'short', timeStyle: 'medium'});
    const KINDS: Record<string, string> = {C: 'copied', R: 'replaced', D: 'deleted', E: 'error'};
    const shown = $derived(lines.filter((l) => filter === 'all' || l.kind === filter || (filter === 'C' && l.kind === 'R')));
    const result = $derived(rec ? (rec.result in RUN_PILLS ? rec.result : 'failed') : 'failed');
</script>

<Dialog open title={rec ? (rec.dry_run ? t('Tried: {name}', {name: rec.job_name}) : t('Run: {name}', {name: rec.job_name})) : t('Run')} width="46rem" {onclose} testId="run-dialog">
    {#if error}<p class="sy-error" role="alert">{error}</p>{/if}
    {#if rec}
        <div class="sy-run-head">
            <span class="sy-pill {runPillClass(result)}" data-testid="run-pill" data-state={result}><span>{t(RUN_PILLS[result] ?? 'failed')}</span></span>
            <span>{when(rec.start)} · {duration(rec.start, rec.end)} · {rec.tool} · {rec.trigger === 'page' ? t('started here') : t('by its timer')}</span>
        </div>
        {#if rec.dry_run}
            <p class="sy-notice" data-testid="run-dry">{t('Nothing was copied or deleted. A run would copy {copied} files, replace {replaced} and delete {deleted} ({size}).', {copied: rec.copied, replaced: rec.replaced, deleted: rec.deleted, size: formatSize(rec.bytes)})}</p>
        {:else}
            <p data-testid="run-summary">{t('{copied} copied, {replaced} replaced, {deleted} deleted, {errors} errors · {size}', {copied: rec.copied, replaced: rec.replaced, deleted: rec.deleted, errors: rec.errors, size: formatSize(rec.bytes)})}</p>
        {/if}
        {#if released}<p class="sy-notice" data-testid="run-released">{t('You have looked at what this mirror job would do: it may run now, and its timer is switched on if it has a schedule.')}</p>{/if}
        {#if rec.error}<p class="sy-error" data-testid="run-error">{rec.error}</p>{/if}
        <div class="sy-row">
            <label class="sy-label" for="sy-run-filter">{t('Show')}</label>
            <select id="sy-run-filter" class="hmm-select" bind:value={filter} data-testid="run-filter">
                <option value="all">{t('everything')}</option>
                <option value="C">{t('copied')}</option>
                <option value="D">{t('deleted')}</option>
                <option value="E">{t('errors')}</option>
            </select>
            <span class="sy-muted">{t('{n} lines', {n: shown.length})}{#if more || rec.log_cut}{' · '}{t('the list is cut')}{/if}</span>
        </div>
        <ul class="sy-runlist" data-testid="run-lines">
            {#each shown as l, i (i)}
                <li data-kind={l.kind}><span class="sy-runkind sy-runkind-{l.kind}">{t(KINDS[l.kind] ?? 'error')}</span> <span class="sy-mono">{l.text}</span></li>
            {:else}
                <li class="sy-muted">{t('Nothing to list.')}</li>
            {/each}
        </ul>
    {:else if !error}
        <p class="sy-muted">{t('Loading…')}</p>
    {/if}
    {#snippet buttons()}
        <button class="hmm-button sy-primary" onclick={onclose} data-testid="run-close">{t('Close')}</button>
    {/snippet}
</Dialog>
