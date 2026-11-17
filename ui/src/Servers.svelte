<script lang="ts">
    // The SSH servers copy jobs send folders to with rsync: a place, a key the addon made for it,
    // and the server's host key - read, shown, and trusted only after the user compared it.
    import {t} from 'occulite-ui/i18n';
    import type {NoticesStore} from 'occulite-ui/notices';

    import ConfirmDialog from './ConfirmDialog.svelte';
    import ServerDialog from './ServerDialog.svelte';
    import {ApiError, del, post} from './api.js';
    import {SERVER_PILLS, serverPillClass, type Server} from './model.js';

    let {servers, notices, onchange}: {servers: Server[]; notices: NoticesStore; onchange: () => void} = $props();

    let busy = $state('');
    let dialog = $state<{server?: Server; setup?: boolean} | null>(null);
    let removing = $state<Server | null>(null);

    const pill = (s: Server) => (busy === s.id + 'test' ? 'testing' : s.state in SERVER_PILLS ? s.state : 'error');

    async function test(s: Server) {
        busy = s.id + 'test';
        try {
            const r = await post<Server>(`api/v1/servers/${s.id}/test`);
            if (r.state === 'ok') notices.push('info', t('{name} answers.', {name: r.name}));
            onchange();
        } catch (e) {
            notices.push('error', (e as Error).message);
        } finally {
            busy = '';
        }
    }

    async function remove() {
        if (!removing) return;
        const s = removing;
        try {
            await del(`api/v1/servers/${s.id}`);
            notices.push('info', t('{name} was removed. Nothing on the server was deleted.', {name: s.name}));
        } catch (e) {
            const body = e instanceof ApiError ? (e.body as {error?: string; jobs?: string[]}) : undefined;
            notices.push('error', body?.error === 'in-use' ? t('A copy job copies to this server: delete the job first ({names}).', {names: (body.jobs ?? []).join(', ')}) : (e as Error).message);
        } finally {
            removing = null;
            onchange();
        }
    }
</script>

<section class="sy-section" id="servers">
    <div class="sy-section-head">
        <h2>{t('SSH servers')}</h2>
        <button class="hmm-button" onclick={() => (dialog = {})} data-testid="server-add">{t('Add an SSH server…')}</button>
    </div>
    <p class="sy-lead">{t('A server reached over SSH, for copy jobs with rsync - a NAS, another computer. The login is by a key this system makes; there is no password. Backups of the system go to an SSH server on the Backup page.')}</p>
    <div class="sy-cards">
        {#each servers as s (s.id)}
            {@const p = pill(s)}
            <div class="sy-card" class:bad={serverPillClass(p) === 'bad'} data-testid="server" data-server={s.id}>
                <div class="sy-card-head">
                    <div class="sy-card-title" title={`${s.name} · ${s.user}@${s.host}:${s.path}`}>{s.name} <span class="sy-detail">· {s.user}@{s.host}{s.port !== 22 ? `:${s.port}` : ''} · {s.path}</span></div>
                    <span class="sy-pill {serverPillClass(p)}" data-testid="server-pill" data-state={p}><span>{t(SERVER_PILLS[p] ?? 'error')}</span>{#each Object.values(SERVER_PILLS) as label (label)}<span class="sy-pill-size" aria-hidden="true">{t(label)}</span>{/each}</span>
                </div>
                <div class="sy-card-lines">
                    {#if s.state !== 'ok' && s.text}<div class="sy-muted" data-testid="server-text">{s.text}</div>{/if}
                    {#if s.host_keys.length}
                        <div class="sy-muted sy-mono" title={s.host_keys.map((k) => `${k.type} ${k.fingerprint}`).join('\n')}>{t('Host key: {fp}', {fp: s.host_keys[0]!.fingerprint})}</div>
                    {:else}
                        <div data-testid="server-nohostkey">{t("The server's host key is not confirmed yet.")}</div>
                    {/if}
                    <div class="sy-muted">{s.jobs.length ? t('Used by the copy job {names}.', {names: s.jobs.map((n) => `"${n}"`).join(', ')}) : t('Not used by a copy job yet.')}</div>
                </div>
                <div class="sy-card-foot">
                    <button class="hmm-button sy-sized" onclick={() => void test(s)} disabled={busy !== ''} data-testid="server-test"><span>{busy === s.id + 'test' ? t('Testing…') : t('Test')}</span><span class="sy-size" aria-hidden="true">{t('Testing…')}</span><span class="sy-size" aria-hidden="true">{t('Test')}</span></button>
                    <button class="hmm-button" onclick={() => (dialog = {server: s, setup: true})} disabled={busy !== ''} data-testid="server-setup">{t('Key and host key…')}</button>
                    <button class="hmm-button" onclick={() => (dialog = {server: s})} disabled={busy !== ''} data-testid="server-edit">{t('Edit')}</button>
                    <button class="hmm-button" onclick={() => (removing = s)} disabled={busy !== ''} data-testid="server-remove">{t('Remove')}</button>
                </div>
            </div>
        {/each}
    </div>
</section>

{#if dialog}
    <ServerDialog server={dialog.server} setup={dialog.setup} onclose={() => { dialog = null; onchange(); }} />
{/if}

<ConfirmDialog
    open={removing !== null}
    title={t('Remove the SSH server')}
    message={t('Remove the server "{name}"? Its key is deleted on this system; nothing on the server is touched.', {name: removing?.name ?? ''})}
    confirmLabel={t('Remove')}
    danger
    testId="server-remove-dialog"
    onconfirm={() => void remove()}
    onclose={() => (removing = null)}
/>
