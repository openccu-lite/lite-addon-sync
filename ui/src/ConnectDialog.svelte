<script lang="ts">
    // Connecting an account, or logging one in again. Accounts with keys (S3, WebDAV, a Google
    // service account) are one form. OneDrive, Google Drive and Dropbox log in at their provider:
    // three numbered steps - open the login, copy the address of the page that cannot be loaded,
    // paste it here - or, for who has rclone on a computer, its own command and what it prints.
    import {Dialog} from 'occulite-ui';
    import {copyText} from 'occulite-ui/clipboard';
    import {t} from 'occulite-ui/i18n';

    import {post, put} from './api.js';
    import {COMPANY, cleanSpec, looksLikeRedirect, specReady, type Account, type Kind, type Login, type Spec} from './model.js';

    let {kinds, account, pending, onclose, ondone}: {kinds: Kind[]; account?: Account; pending?: Login; onclose: () => void; ondone: (a: Account) => void} = $props();

    // svelte-ignore state_referenced_locally
    let spec = $state<Spec>({name: account?.name ?? '', kind: account?.kind ?? kinds[0]?.id ?? 'onedrive', provider: 'Other', vendor: 'nextcloud'});
    // svelte-ignore state_referenced_locally
    let serviceAccount = $state(account?.auth === 'service-account');
    let way = $state<'browser' | 'rclone'>('browser');
    // svelte-ignore state_referenced_locally
    let flow = $state<{id: string; url: string; command?: string} | undefined>(pending && !account ? undefined : undefined);
    let address = $state('');
    let token = $state('');
    let error = $state('');
    let busy = $state(false);
    let host = $state<HTMLElement | undefined>();
    let copied = $state(false);

    const kind = $derived(kinds.find((k) => k.id === spec.kind));
    const label = $derived(kind?.label ?? spec.kind);
    const login = $derived(!!kind?.oauth && !serviceAccount);
    const ready = $derived(specReady(spec, serviceAccount));
    const command = $derived(spec.client_id ? `rclone authorize "${spec.kind}" "${spec.client_id}" "${spec.client_secret ?? ''}"` : `rclone authorize "${spec.kind}"`);

    function fail(e: unknown) {
        error = (e as Error).message;
    }

    // an account with keys: one request
    async function save() {
        busy = true;
        error = '';
        try {
            const body = cleanSpec(spec, serviceAccount);
            ondone(account ? await put<Account>(`api/v1/accounts/${account.id}/credentials`, body) : await post<Account>('api/v1/accounts', body));
        } catch (e) {
            fail(e);
        } finally {
            busy = false;
        }
    }

    // step 1: the tab is opened by the click itself (a browser blocks one opened later), and gets
    // its address once the system has started the login
    async function openLogin() {
        busy = true;
        error = '';
        const tab = window.open('about:blank', '_blank');
        try {
            const r = await post<{id: string; url: string; command: string}>('api/v1/login', {...cleanSpec(spec, false), ...(account ? {account: account.id} : {})});
            flow = r;
            if (tab) tab.location.href = r.url;
        } catch (e) {
            tab?.close();
            fail(e);
        } finally {
            busy = false;
        }
    }

    async function finish() {
        busy = true;
        error = '';
        try {
            const body = way === 'browser' ? {id: flow?.id, address: address.trim()} : {token: token.trim(), spec: cleanSpec(spec, false), ...(account ? {account: account.id} : {})};
            ondone(await post<Account>('api/v1/login/finish', body));
        } catch (e) {
            fail(e);
        } finally {
            busy = false;
        }
    }

    async function cancel() {
        if (flow) {
            try {
                await fetch(new URL('api/v1/login', document.baseURI), {method: 'DELETE', headers: {'X-CCU-Addon-Request': '1'}, credentials: 'same-origin'});
            } catch {
                /* the login ends by itself after ten minutes */
            }
        }
        onclose();
    }

    async function copy(text: string) {
        const how = await copyText(text, {host});
        copied = how !== 'failed';
        setTimeout(() => (copied = false), 2000);
    }

    function kindChanged() {
        flow = undefined;
        address = '';
        token = '';
        error = '';
        serviceAccount = false;
    }
</script>

<Dialog open title={account ? (login ? t('Log in again: {name}', {name: account.name}) : t('Change the credentials: {name}', {name: account.name})) : t('Connect an account')} width="36rem" height="38rem" onclose={() => void cancel()} testId="connect-dialog">
    <div bind:this={host}>
        <form class="sy-form" onsubmit={(e) => e.preventDefault()}>
            {#if !account}
                <label class="sy-label" for="sy-kind">{t('Kind')}</label>
                <select id="sy-kind" class="hmm-select" bind:value={spec.kind} onchange={kindChanged} disabled={!!flow} data-testid="kind">
                    {#each kinds as k (k.id)}<option value={k.id}>{k.label}</option>{/each}
                </select>
                <label class="sy-label" for="sy-name">{t('Name')}</label>
                <input id="sy-name" type="text" class="hmm-input" bind:value={spec.name} maxlength="64" placeholder={label} disabled={!!flow} data-testid="name" />
            {/if}

            {#if spec.kind === 'drive'}
                <p class="sy-notice sy-span" data-testid="drive-note">
                    {t('Google Drive needs a client ID of your own: the one rclone shares with all its users is being retired. Create one in the Google Cloud console (an OAuth client of the type "Desktop app") and publish the app, or the login lapses after 7 days.')}
                    <a href="https://rclone.org/drive/#making-your-own-client-id" target="_blank" rel="noreferrer noopener">{t('Step by step (rclone.org)')}</a>
                </p>
                <label class="sy-span"><input type="checkbox" bind:checked={serviceAccount} disabled={!!flow || !!account} data-testid="service-account" /> {t('Use a service account instead (no login in a browser)')}</label>
            {/if}

            {#if login}
                <label class="sy-label" for="sy-client-id">{t('Client ID')}</label>
                <input id="sy-client-id" type="text" class="hmm-input" bind:value={spec.client_id} autocomplete="off" placeholder={spec.kind === 'drive' ? '' : t('(optional)')} disabled={!!flow} data-testid="client-id" />
                <label class="sy-label" for="sy-client-secret">{t('Client secret')}</label>
                <input id="sy-client-secret" type="password" class="hmm-input" bind:value={spec.client_secret} autocomplete="new-password" placeholder={spec.kind === 'drive' ? '' : t('(optional)')} disabled={!!flow} data-testid="client-secret" />
                {#if spec.kind === 'dropbox'}
                    <p class="sy-help sy-span">{t('Without one, rclone\'s own Dropbox app is used. An app of your own of the type "App folder" confines this system to one folder of your Dropbox.')}</p>
                {:else if spec.kind === 'onedrive'}
                    <p class="sy-help sy-span">{t('Without one, rclone\'s own registration is used. A OneDrive login lapses after 90 days without an upload.')}</p>
                {/if}
            {:else if spec.kind === 'drive'}
                <label class="sy-label" for="sy-sa">{t('Key file (JSON)')}</label>
                <textarea id="sy-sa" class="hmm-input" rows="5" bind:value={spec.service_account} spellcheck="false" placeholder={'{"type": "service_account", …}'} data-testid="service-account-key"></textarea>
                <label class="sy-label" for="sy-folder-id">{t('Folder ID')}</label>
                <input id="sy-folder-id" type="text" class="hmm-input" bind:value={spec.folder_id} autocomplete="off" data-testid="folder-id" />
                <p class="sy-help sy-span">{t("A service account has no storage of its own: share a folder of your Google Drive with the account's address (as an editor) and enter the last part of that folder's address.")}</p>
            {:else if spec.kind === 's3'}
                <label class="sy-label" for="sy-provider">{t('Provider')}</label>
                <select id="sy-provider" class="hmm-select" bind:value={spec.provider} data-testid="provider">
                    {#each ['AWS', 'Minio', 'Wasabi', 'Cloudflare', 'DigitalOcean', 'Scaleway', 'Hetzner', 'IONOS', 'Other'] as p (p)}<option value={p}>{p === 'Other' ? t('Other (S3 compatible)') : p}</option>{/each}
                </select>
                <label class="sy-label" for="sy-endpoint">{t('Endpoint')}</label>
                <input id="sy-endpoint" type="text" class="hmm-input" bind:value={spec.endpoint} placeholder={spec.provider === 'AWS' ? t('(optional)') : 'https://s3.example.org'} autocomplete="off" data-testid="endpoint" />
                <label class="sy-label" for="sy-region">{t('Region')}</label>
                <input id="sy-region" type="text" class="hmm-input" bind:value={spec.region} placeholder={t('(optional)')} autocomplete="off" data-testid="region" />
                <label class="sy-label" for="sy-bucket">{t('Bucket')}</label>
                <input id="sy-bucket" type="text" class="hmm-input" bind:value={spec.bucket} autocomplete="off" data-testid="bucket" />
                <label class="sy-label" for="sy-access">{t('Access key')}</label>
                <input id="sy-access" type="text" class="hmm-input" bind:value={spec.access_key} autocomplete="off" data-testid="access-key" />
                <label class="sy-label" for="sy-secret">{t('Secret key')}</label>
                <input id="sy-secret" type="password" class="hmm-input" bind:value={spec.secret_key} autocomplete="new-password" data-testid="secret-key" />
                <p class="sy-help sy-span">{t('The bucket must exist. Use a key that may read and write this bucket and nothing else.')}</p>
            {:else if spec.kind === 'webdav'}
                <label class="sy-label" for="sy-url">{t('Address')}</label>
                <input id="sy-url" type="text" class="hmm-input" bind:value={spec.url} placeholder="https://cloud.example.org/remote.php/dav/files/name" autocomplete="off" data-testid="url" />
                <label class="sy-label" for="sy-vendor">{t('Server')}</label>
                <select id="sy-vendor" class="hmm-select" bind:value={spec.vendor} data-testid="vendor">
                    <option value="nextcloud">Nextcloud</option><option value="owncloud">ownCloud</option><option value="other">{t('Other WebDAV server')}</option>
                </select>
                <label class="sy-label" for="sy-user">{t('User')}</label>
                <input id="sy-user" type="text" class="hmm-input" bind:value={spec.user} autocomplete="off" data-testid="user" />
                <label class="sy-label" for="sy-password">{t('Password')}</label>
                <input id="sy-password" type="password" class="hmm-input" bind:value={spec.password} autocomplete="new-password" data-testid="password" />
                <p class="sy-help sy-span">{t('Nextcloud: use an app password (Settings → Security), not the password of the account.')}</p>
            {/if}
        </form>

        {#if login}
            <p class="sy-notice" data-testid="privacy">{t('{company} receives the backups of this system. They are encrypted before they leave it; {company} sees their names and sizes, when they are made, and the address this system has on the internet.', {company: COMPANY[spec.kind] ?? label})}</p>
            {#if way === 'browser'}
                <ol class="sy-steps" data-testid="steps">
                    <li>
                        {t("Open {label}'s login and allow the access.", {label})}
                        <div><button class="hmm-button sy-primary" onclick={() => void openLogin()} disabled={busy || !ready} data-testid="open-login">{flow ? t('Open the login again') : t("Open {label}'s login", {label})}</button></div>
                    </li>
                    <li>{t('After the login the browser shows a page that cannot be loaded, at an address that begins with http://localhost:53682/. That is expected: copy the whole address from the address bar.')}</li>
                    <li>
                        {t('Paste the address here.')}
                        <input type="text" class="hmm-input" style="width: 100%; box-sizing: border-box" bind:value={address} placeholder="http://localhost:53682/?state=…&code=…" autocomplete="off" disabled={!flow} data-testid="address" />
                    </li>
                </ol>
                <p class="sy-help"><button class="sy-linkbutton" onclick={() => { way = 'rclone'; error = ''; }} data-testid="way-rclone">{t('I have rclone on this computer instead')}</button></p>
            {:else}
                <ol class="sy-steps" data-testid="steps-rclone">
                    <li>
                        {t('Run this on a computer with a browser and rclone, and log in:')}
                        <code class="sy-code" data-testid="command">{command}</code>
                        <div><button class="hmm-button sy-sized" onclick={() => void copy(command)} data-testid="copy-command"><span>{copied ? t('Copied.') : t('Copy')}</span><span class="sy-size" aria-hidden="true">{t('Copied.')}</span><span class="sy-size" aria-hidden="true">{t('Copy')}</span></button></div>
                    </li>
                    <li>
                        {t('rclone prints a block of text between two marker lines. Paste it here.')}
                        <textarea class="hmm-input" style="width: 100%; box-sizing: border-box" rows="4" bind:value={token} spellcheck="false" data-testid="token"></textarea>
                    </li>
                </ol>
                <p class="sy-help"><button class="sy-linkbutton" onclick={() => { way = 'browser'; error = ''; }} data-testid="way-browser">{t('Log in with this browser instead')}</button></p>
            {/if}
        {/if}
        <p class="sy-error" role="alert" data-testid="connect-error">{error}</p>
    </div>
    {#snippet buttons()}
        <button class="hmm-button" onclick={() => void cancel()}>{t('Cancel')}</button>
        {#if login}
            <button class="hmm-button sy-primary sy-sized" onclick={() => void finish()} disabled={busy || !ready || (way === 'browser' ? !flow || !looksLikeRedirect(address) : !token.trim())} data-testid="finish"><span>{busy && (address || token) ? t('Connecting…') : t('Finish')}</span><span class="sy-size" aria-hidden="true">{t('Connecting…')}</span><span class="sy-size" aria-hidden="true">{t('Finish')}</span></button>
        {:else}
            <button class="hmm-button sy-primary sy-sized" onclick={() => void save()} disabled={busy || !ready} data-testid="save"><span>{busy ? t('Connecting…') : account ? t('Save') : t('Connect')}</span><span class="sy-size" aria-hidden="true">{t('Connecting…')}</span><span class="sy-size" aria-hidden="true">{t('Connect')}</span></button>
        {/if}
    {/snippet}
</Dialog>
