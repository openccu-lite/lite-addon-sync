<script lang="ts">
    // An SSH server: its form (new or edited), and then the three steps that make it usable - the
    // key's line for the server's authorized_keys, the server's host key read and confirmed, the
    // test. Nothing is trusted unseen: the fingerprints are shown, and "Trust" names exactly them.
    import {Dialog} from 'occulite-ui';
    import {copyText} from 'occulite-ui/clipboard';
    import {t} from 'occulite-ui/i18n';

    import ConfirmDialog from './ConfirmDialog.svelte';
    import {post, put} from './api.js';
    import {SERVER_PILLS, serverPillClass, type Server} from './model.js';

    let {server, setup = false, onclose}: {server?: Server; setup?: boolean; onclose: () => void} = $props();

    // svelte-ignore state_referenced_locally
    let current = $state<Server | undefined>(server);
    // svelte-ignore state_referenced_locally
    let step = $state<'form' | 'setup'>(server && setup ? 'setup' : 'form');
    // svelte-ignore state_referenced_locally
    let form = $state({name: server?.name ?? '', host: server?.host ?? '', port: server?.port ?? 22, user: server?.user ?? '', path: server?.path ?? ''});
    let busy = $state('');
    let error = $state('');
    let copied = $state(false);
    let host = $state<HTMLElement | undefined>();
    let newKey = $state(false);

    const ready = $derived(!!form.name.trim() && !!form.host.trim() && !!form.user.trim() && !!form.path.trim());

    async function act(what: string, f: () => Promise<Server>) {
        busy = what;
        error = '';
        try {
            current = await f();
            return true;
        } catch (e) {
            error = (e as Error).message;
            return false;
        } finally {
            busy = '';
        }
    }

    async function save() {
        const body = {name: form.name.trim(), host: form.host.trim(), port: Number(form.port) || 22, user: form.user.trim(), path: form.path.trim()};
        const ok = await act('save', () => (current ? put<Server>(`api/v1/servers/${current.id}`, body) : post<Server>('api/v1/servers', body)));
        if (ok) step = 'setup';
    }

    async function copyKey() {
        if (!current) return;
        copied = (await copyText(current.public_key, {host})) !== 'failed';
        setTimeout(() => (copied = false), 2000);
    }

    const scan = () => act('scan', () => post<Server>(`api/v1/servers/${current!.id}/hostkey`));
    const trust = () => act('trust', () => put<Server>(`api/v1/servers/${current!.id}/hostkey`, {fingerprints: (current!.pending ?? []).map((k) => k.fingerprint)}));
    const test = () => act('test', () => post<Server>(`api/v1/servers/${current!.id}/test`));
    async function replaceKey() {
        newKey = false;
        await act('key', () => post<Server>(`api/v1/servers/${current!.id}/key`));
    }
    const pillState = $derived(busy === 'test' ? 'testing' : current && current.state in SERVER_PILLS ? current.state : 'error');
</script>

<Dialog open title={step === 'form' ? (current ? t('Edit the SSH server') : t('Add an SSH server')) : t('SSH server: {name}', {name: current?.name ?? ''})} width="38rem" {onclose} testId="server-dialog">
    <div bind:this={host}>
        {#if step === 'form'}
            <form class="sy-form" onsubmit={(e) => { e.preventDefault(); if (ready) void save(); }}>
                <label class="sy-label" for="sy-srv-name">{t('Name')}</label>
                <input id="sy-srv-name" type="text" class="hmm-input" bind:value={form.name} maxlength="60" data-testid="server-name" />
                <label class="sy-label" for="sy-srv-host">{t('Host')}</label>
                <input id="sy-srv-host" type="text" class="hmm-input" bind:value={form.host} placeholder="nas.example.org" autocomplete="off" spellcheck="false" data-testid="server-host" />
                <label class="sy-label" for="sy-srv-port">{t('Port')}</label>
                <input id="sy-srv-port" type="number" class="hmm-input" min="1" max="65535" bind:value={form.port} data-testid="server-port" />
                <label class="sy-label" for="sy-srv-user">{t('User')}</label>
                <input id="sy-srv-user" type="text" class="hmm-input" bind:value={form.user} autocomplete="off" spellcheck="false" data-testid="server-user" />
                <label class="sy-label" for="sy-srv-path">{t('Base path')}</label>
                <input id="sy-srv-path" type="text" class="hmm-input" bind:value={form.path} placeholder="/volume1/openccu-lite" autocomplete="off" spellcheck="false" data-testid="server-path" />
                <p class="sy-help sy-span">{t('The folder on the server every copy job writes below; it must exist. A dedicated account that may write only there is the safest.')}</p>
            </form>
        {:else if current}
            <ol class="sy-steps">
                <li>
                    <strong>{t('Allow this key on the server.')}</strong>
                    <p>{t("Add this line to the file ~/.ssh/authorized_keys of the account {user} on the server. The word restrict at its start keeps the key from opening a terminal or a tunnel.", {user: current.user})}</p>
                    <pre class="sy-code" data-testid="server-pubkey">{current.public_key}</pre>
                    <div class="sy-row">
                        <button class="hmm-button" onclick={() => void copyKey()} data-testid="server-copykey">{copied ? t('Copied.') : t('Copy')}</button>
                        <button class="sy-linkbutton" onclick={() => (newKey = true)} disabled={busy !== ''} data-testid="server-newkey">{t('Make a new key')}</button>
                    </div>
                </li>
                <li>
                    <strong>{t("Confirm the server's host key.")}</strong>
                    {#if current.host_keys.length && !current.pending?.length}
                        <p>{t('Confirmed:')}</p>
                        {#each current.host_keys as k (k.fingerprint)}<div class="sy-mono" data-testid="server-hostkey">{k.type} {k.fingerprint}</div>{/each}
                        <div class="sy-row"><button class="sy-linkbutton" onclick={() => void scan()} disabled={busy !== ''} data-testid="server-rescan">{t('Read the host key again')}</button></div>
                    {:else if current.pending?.length}
                        <p>{t('The server shows these keys. Compare them with what the server itself says (ssh-keygen -lf on its host key files) and trust them only when they are the same:')}</p>
                        {#each current.pending as k (k.fingerprint)}<div class="sy-mono" data-testid="server-pending">{k.type} {k.fingerprint}</div>{/each}
                        {#if current.host_keys.length}<p class="sy-error">{t('These are not the keys confirmed before. If the server was not set up anew, someone may be between this system and the server.')}</p>{/if}
                        <div class="sy-row"><button class="hmm-button sy-primary" onclick={() => void trust()} disabled={busy !== ''} data-testid="server-trust">{t('Trust these keys')}</button></div>
                    {:else}
                        <p>{t('The system reads the key the server shows; nothing is trusted before you have seen it.')}</p>
                        <div class="sy-row"><button class="hmm-button" onclick={() => void scan()} disabled={busy !== ''} data-testid="server-scan">{busy === 'scan' ? t('Reading…') : t('Read the host key')}</button></div>
                    {/if}
                </li>
                <li>
                    <strong>{t('Test it.')}</strong>
                    <p>{t('The test writes a small file below the base path with rsync and removes it again.')}</p>
                    <div class="sy-row">
                        <button class="hmm-button" onclick={() => void test()} disabled={busy !== '' || !current.host_keys.length} data-testid="server-dialog-test">{t('Test')}</button>
                        <span class="sy-pill {serverPillClass(pillState)}" data-testid="server-dialog-pill" data-state={pillState}><span>{t(SERVER_PILLS[pillState] ?? 'error')}</span>{#each Object.values(SERVER_PILLS) as label (label)}<span class="sy-pill-size" aria-hidden="true">{t(label)}</span>{/each}</span>
                    </div>
                    {#if current.state !== 'ok' && current.text}<p class="sy-muted" data-testid="server-dialog-text">{current.text}</p>{/if}
                </li>
            </ol>
        {/if}
        {#if error}<p class="sy-error" role="alert" data-testid="server-error">{error}</p>{/if}
    </div>
    {#snippet buttons()}
        {#if step === 'form'}
            <button class="hmm-button" onclick={onclose}>{t('Cancel')}</button>
            <button class="hmm-button sy-primary" onclick={() => void save()} disabled={busy !== '' || !ready} data-testid="server-save">{current ? t('Save') : t('Add')}</button>
        {:else}
            <button class="hmm-button sy-primary" onclick={onclose} data-testid="server-done">{t('Done')}</button>
        {/if}
    {/snippet}
</Dialog>

<ConfirmDialog
    open={newKey}
    title={t('Make a new key')}
    message={t('The key in use stops working at once: every copy job to this server fails until the new line is in the authorized_keys of the server.')}
    confirmLabel={t('Make a new key')}
    danger
    testId="server-newkey-dialog"
    onconfirm={() => void replaceKey()}
    onclose={() => (newKey = false)}
/>
