<script lang="ts">
    // The page: the accounts - connect, test, log in again, edit, remove - and where each is used.
    // The system's backup is set up on the Backup page; this page says so and leads there.
    import {onMount} from 'svelte';
    import {Dialog, Notices, Tabs, ThemeSwitch} from 'occulite-ui';
    import {i18n, t} from 'occulite-ui/i18n';
    import {NoticesStore} from 'occulite-ui/notices';
    import {cycleTheme, theme} from 'occulite-ui/theme';

    import ConfirmDialog from './ConfirmDialog.svelte';
    import ConnectDialog from './ConnectDialog.svelte';
    import History from './History.svelte';
    import Jobs from './Jobs.svelte';
    import Servers from './Servers.svelte';
    import {ApiError, del, get, post, put} from './api.js';
    import {PILLS, formatBytes, pillClass, pillOf, useForBackupsHref, type Account, type State, type Target} from './model.js';

    const notices = new NoticesStore();
    let view = $state<State | undefined>();
    let loadError = $state('');
    let busy = $state('');
    // the dialogs
    let connect = $state<{account?: Account} | null>(null);
    let editing = $state<Account | null>(null);
    let editName = $state('');
    let editLimit = $state(0);
    let removing = $state<{account: Account; targets: Target[]} | null>(null);
    // the tabs: Accounts (with the SSH servers), Copy jobs, History; the address's #… names one
    const TABS = ['accounts', 'jobs', 'history'];
    const fromHash = () => {
        const h = location.hash.replace('#', '');
        return h === 'servers' ? 'accounts' : TABS.includes(h) ? h : 'accounts';
    };
    let tab = $state(fromHash());
    let tick = $state(0);
    function selectTab(id: string) {
        tab = id;
        history.replaceState(null, '', '#' + id);
    }

    async function load(refresh = false) {
        try {
            view = await get<State>('api/v1/state' + (refresh ? '?refresh=1' : ''));
            loadError = '';
        } catch (e) {
            loadError = (e as Error).message;
        }
    }

    onMount(() => {
        // opening the page is a look at the system as it is now, not at an answer a few seconds old
        void load(true);
        // every 10 s, and every 2 s while a copy job runs, so its figures move
        let n = 0;
        const poll = setInterval(() => {
            n++;
            if (n % 5 === 0 || view?.copy?.jobs.some((j) => j.running)) void load().then(() => tick++);
        }, 2000);
        const onHash = () => (tab = fromHash());
        window.addEventListener('hashchange', onHash);
        return () => {
            clearInterval(poll);
            window.removeEventListener('hashchange', onHash);
        };
    });

    const oauthKinds = $derived((view?.kinds ?? []).filter((k) => k.oauth).map((k) => k.id));
    const isOAuth = (a: Account) => oauthKinds.includes(a.kind) && a.auth !== 'service-account';
    const pill = (a: Account) => (busy === a.id + 'test' ? 'testing' : pillOf(a, oauthKinds));
    const when = (iso?: string) => (iso ? new Date(iso).toLocaleString(i18n.language === 'de' ? 'de-DE' : 'en-GB', {dateStyle: 'short', timeStyle: 'short'}) : '');

    function replace(a: Account) {
        if (!view) return;
        const i = view.accounts.findIndex((x) => x.id === a.id);
        if (i >= 0) view.accounts[i] = a;
        else view.accounts.push(a);
    }

    async function test(a: Account) {
        busy = a.id + 'test';
        try {
            const r = await post<Account>(`api/v1/accounts/${a.id}/test`);
            replace(r);
            if (r.state === 'writable') notices.push('info', t('{name} answers.', {name: r.name}));
        } catch (e) {
            notices.push('error', (e as Error).message);
        } finally {
            busy = '';
        }
    }

    function edit(a: Account) {
        editing = a;
        editName = a.name;
        editLimit = a.upload_limit_kbps;
    }

    async function saveEdit() {
        if (!editing) return;
        busy = 'edit';
        try {
            replace(await put<Account>(`api/v1/accounts/${editing.id}`, {name: editName.trim(), upload_limit_kbps: Math.max(0, Math.floor(Number(editLimit) || 0))}));
            editing = null;
        } catch (e) {
            notices.push('error', (e as Error).message);
        } finally {
            busy = '';
        }
    }

    async function remove(force = false) {
        if (!removing) return;
        const a = removing.account;
        busy = a.id + 'remove';
        try {
            await del(`api/v1/accounts/${a.id}${force ? '?force=1' : ''}`);
            removing = null;
            notices.push('info', t('{name} was removed. Nothing in the account itself was deleted.', {name: a.name}));
            await load(true);
        } catch (e) {
            // a backup target uses the account: the question names it, and asks again; a copy job
            // that copies to it is deleted first
            const body = e instanceof ApiError ? (e.body as {error?: string; targets?: Target[]; jobs?: string[]}) : undefined;
            if (body?.error === 'in-use' && body.targets) removing = {account: a, targets: body.targets};
            else if (body?.error === 'in-use' && body.jobs) {
                removing = null;
                notices.push('error', t('A copy job copies to this account: delete the job first ({names}).', {names: body.jobs.join(', ')}));
            } else notices.push('error', (e as Error).message);
        } finally {
            busy = '';
        }
    }

    function connected(a: Account) {
        connect = null;
        notices.push('info', t('{name} is connected.', {name: a.name}));
        void load(true);
    }
</script>

<div class="sy-app" lang={i18n.language}>
    <header class="sy-header">
        <h1>Sync</h1>
        <span class="sy-spacer"></span>
        {#if view}<span class="sy-muted" title={t('rclone {v}', {v: view.rclone || '?'})} data-testid="version">{t('Version {v}', {v: view.version})} · {t('rclone {v}', {v: view.rclone || '?'})}</span>{/if}
        {#if !theme.followsShell}
            <ThemeSwitch theme={theme.choice} onclick={cycleTheme} />
        {/if}
    </header>
    <div class="sy-banners">
        {#if loadError}
            <div class="sy-banner sy-banner-error" role="alert">{t('The page could not read its state: {error}', {error: loadError})}</div>
        {/if}
        {#if view && !view.rclone}
            <div class="sy-banner sy-banner-error" role="alert" data-testid="banner-rclone">{t('The transfer program (rclone) of this addon does not run. Install the addon again.')}</div>
        {/if}
        {#if view && (!view.provider || (view.system.reachable && !view.system.provider))}
            <div class="sy-banner sy-banner-warn" role="status" data-testid="banner-provider">{t('This system does not offer the accounts as backup targets: it needs openccu-lite 2.0.0-beta.7 or newer.')}</div>
        {:else if view?.system.reachable && !view.system.encryption}
            <div class="sy-banner sy-banner-warn" role="status" data-testid="banner-encryption">
                {t('Backup encryption is not set up on this system. Backups go to an account encrypted only: set up encryption before you add a cloud target.')}
                <a href="/backup#encryption" target="_top">{t('Backup page')}</a>
            </div>
        {/if}
    </div>
    <main class="sy-page">
        <Tabs tabs={[{id: 'accounts', label: t('Accounts')}, {id: 'jobs', label: t('Copy jobs'), badge: view?.copy?.jobs.length || undefined}, {id: 'history', label: t('History')}]} active={tab} onselect={selectTab} testId="tabs" />
        {#if tab === 'accounts'}
        <section class="sy-section" id="accounts">
            <div class="sy-section-head">
                <h2>{t('Accounts')}</h2>
                <button class="hmm-button sy-primary" onclick={() => (connect = {})} disabled={!view} data-testid="connect">{t('Connect an account…')}</button>
            </div>
            <p class="sy-lead">
                {t("Accounts are used by the system's backup and by copy jobs. For backups, connect one here, then choose it as a target on the Backup page. Schedule, the number of backups to keep and the restore are set there.")}
                <a href="/backup#targets" target="_top" data-testid="backup-link">{t('Backup page')}</a>
            </p>
            {#if view}
                {#if !view.accounts.length}
                    <div class="sy-empty" data-testid="empty">
                        <p>{t('Connect an account to use it as a backup target.')}</p>
                        <p class="sy-muted">{t('OneDrive, Google Drive, Dropbox, an S3 bucket or a WebDAV server (Nextcloud). Nothing is contacted until you connect one.')}</p>
                    </div>
                {/if}
                <div class="sy-cards">
                    {#each view.accounts as a (a.id)}
                        {@const p = pill(a)}
                        <div class="sy-card" class:bad={pillClass(p) === 'bad'} data-testid="account" data-account={a.id}>
                            <div class="sy-card-head">
                                <div class="sy-card-title" title={`${a.name} · ${a.detail}`}>{a.name} <span class="sy-detail">· {a.detail}</span></div>
                                <span class="sy-pill {pillClass(p)}" data-testid="pill" data-state={p}><span>{t(PILLS[p] ?? 'error')}</span>{#each Object.values(PILLS) as label (label)}<span class="sy-pill-size" aria-hidden="true">{t(label)}</span>{/each}</span>
                            </div>
                            <div class="sy-card-lines">
                                {#if a.state !== 'writable' && a.text}<div class="sy-muted" data-testid="account-text">{a.text}</div>{/if}
                                {#if a.used_by.length}
                                    <div data-testid="used-by">
                                        {t('Used by the backup target {names}.', {names: a.used_by.map((x) => `"${x.name}"`).join(', ')})}
                                        {#if a.last_upload}{' '}{a.last_upload.ok ? t('Last upload {when}.', {when: when(a.last_upload.at)}) : t('The last upload on {when} failed.', {when: when(a.last_upload.at)})}{/if}
                                        {' '}<a href="/backup#targets" target="_top">{t('Backup page')}</a>
                                    </div>
                                {:else}
                                    <div class="sy-muted" data-testid="unused">{t('Not used by a backup target yet.')}</div>
                                {/if}
                                {#if a.jobs?.length}<div data-testid="used-by-jobs">{t('Used by the copy job {names}.', {names: a.jobs.map((n) => `"${n}"`).join(', ')})}</div>{/if}
                                <div class="sy-muted">
                                    {a.upload_limit_kbps > 0 ? t('Upload limit: {n} KiB/s', {n: a.upload_limit_kbps}) : t('Upload limit: none')}{#if a.total_bytes}{' · '}{t('{free} free of {total}', {free: formatBytes(a.free_bytes) || '0 MB', total: formatBytes(a.total_bytes)})}{/if}
                                </div>
                            </div>
                            <div class="sy-card-foot">
                                <button class="hmm-button sy-sized" onclick={() => void test(a)} disabled={busy !== ''} data-testid="test"><span>{busy === a.id + 'test' ? t('Testing…') : t('Test')}</span><span class="sy-size" aria-hidden="true">{t('Testing…')}</span><span class="sy-size" aria-hidden="true">{t('Test')}</span></button>
                                <button class="hmm-button" onclick={() => (connect = {account: a})} disabled={busy !== ''} data-testid="relogin">{isOAuth(a) ? t('Log in again') : t('Change the credentials')}</button>
                                <button class="hmm-button" onclick={() => edit(a)} disabled={busy !== ''} data-testid="edit">{t('Edit')}</button>
                                <button class="hmm-button" onclick={() => { removing = {account: a, targets: []}; }} disabled={busy !== ''} data-testid="remove">{t('Remove')}</button>
                                <span class="sy-spacer"></span>
                                {#if !a.used_by.length && view.provider && view.system.provider}
                                    <a class="hmm-button sy-primary" href={useForBackupsHref(a.id)} target="_top" data-testid="use-for-backups">{t('Use for backups')}</a>
                                {/if}
                            </div>
                        </div>
                    {/each}
                </div>
            {:else if !loadError}
                <p class="sy-muted">{t('Loading…')}</p>
            {/if}
        </section>
        {#if view}<Servers servers={view.servers ?? []} {notices} onchange={() => void load(true)} />{/if}
        {:else if tab === 'jobs'}
            {#if view}<Jobs {view} {notices} onchange={() => void load(true)} />{:else if !loadError}<p class="sy-muted">{t('Loading…')}</p>{/if}
        {:else}
            <History {tick} />
        {/if}
    </main>
    <Notices notices={notices.visible} ondismiss={(id) => notices.dismiss(id)} />
</div>

{#if connect && view}
    <ConnectDialog kinds={view.kinds} account={connect.account} pending={view.login} onclose={() => { connect = null; void load(true); }} ondone={connected} />
{/if}

{#if editing}
<Dialog open title={t('Edit the account')} width="26rem" onclose={() => (editing = null)} testId="edit-dialog">
    <form class="sy-form" onsubmit={(e) => { e.preventDefault(); void saveEdit(); }}>
        <label class="sy-label" for="sy-edit-name">{t('Name')}</label>
        <input id="sy-edit-name" type="text" class="hmm-input" bind:value={editName} maxlength="64" required data-testid="edit-name" />
        <label class="sy-label" for="sy-edit-limit">{t('Upload limit')}</label>
        <input id="sy-edit-limit" type="number" class="hmm-input" min="0" max="1048576" bind:value={editLimit} data-testid="edit-limit" />
        <p class="sy-help sy-span">{t('In KiB/s; 0 = no limit. It applies to the uploads of backups to this account.')}</p>
    </form>
    {#snippet buttons()}
        <button class="hmm-button" onclick={() => (editing = null)}>{t('Cancel')}</button>
        <button class="hmm-button sy-primary" onclick={() => void saveEdit()} disabled={busy !== '' || !editName.trim()} data-testid="edit-save">{t('Save')}</button>
    {/snippet}
</Dialog>
{/if}

<ConfirmDialog
    open={removing !== null}
    title={t('Remove the account')}
    message={removing?.targets.length
        ? t('The backup target {names} uses this account. Remove it all the same? The target then gets no backup until you edit it and choose another account.', {names: removing.targets.map((x) => `"${x.name}"`).join(', ')})
        : t('Remove the account "{name}"? Its login is forgotten on this system; nothing in the account itself is deleted.', {name: removing?.account.name ?? ''})}
    confirmLabel={removing?.targets.length ? t('Remove anyway') : t('Remove')}
    danger
    testId="remove-dialog"
    onconfirm={() => void remove(!!removing?.targets.length)}
    onclose={() => (removing = null)}
/>
