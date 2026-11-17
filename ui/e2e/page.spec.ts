import {expect, test, type Page} from '@playwright/test';

// The page against the daemon with the real rclone, the fake system and a WebDAV server on this
// machine (scripts/dev-serve.sh). The tests run in order, on one worker: they share the daemon's
// accounts. A login at a cloud is walked through against the fake system's stand-in provider; the
// last step, which would contact the real cloud, is answered by the test.

const FAKE = 'http://127.0.0.1:18081';
const DAV = 'http://127.0.0.1:18293/';

async function devSet(page: Page, query: string) {
    const res = await page.request.get(`${FAKE}/dev/set?${query}`);
    expect(res.ok()).toBe(true);
}

async function connectWebDAV(page: Page, name: string, password = 'dev-pass') {
    await page.getByTestId('connect').click();
    const dlg = page.getByTestId('connect-dialog');
    await dlg.getByTestId('kind').selectOption('webdav');
    await dlg.getByTestId('name').fill(name);
    await dlg.getByTestId('url').fill(DAV);
    await dlg.getByTestId('vendor').selectOption('other');
    await dlg.getByTestId('user').fill('backup');
    await dlg.getByTestId('password').fill(password);
    return dlg;
}

// opened on its own (not inside the shell) the page keeps its own language choice
const german = (page: Page) => page.addInitScript(() => localStorage.setItem('sync.language', 'de'));

const account = (page: Page, name: string) => page.getByTestId('account').filter({hasText: name});

test.describe.configure({mode: 'serial'});

test('the empty page: what it is for, the way to the Backup page, German', async ({page}) => {
    await devSet(page, 'encryption=1&provider=1&unused=1');
    await page.goto('./');
    await expect(page.getByRole('heading', {name: 'Accounts'})).toBeVisible();
    await expect(page.getByTestId('empty')).toContainText('Connect an account to use it as a backup target.');
    await expect(page.getByTestId('backup-link')).toHaveAttribute('href', '/backup#targets');
    await expect(page.getByTestId('backup-link')).toHaveAttribute('target', '_top');
    await expect(page.getByTestId('version')).toContainText('rclone v1.');
    await expect(page.getByTestId('banner-provider')).toHaveCount(0);
    await expect(page.getByTestId('banner-encryption')).toHaveCount(0);
    await german(page);
    await page.goto('./');
    await expect(page.getByRole('heading', {name: 'Konten'})).toBeVisible();
    await expect(page.getByTestId('connect')).toHaveText('Konto verbinden…');
    await expect(page.getByTestId('empty')).toContainText('Sicherungsziel');
});

test('the banners: no encryption, a system that does not know the extension', async ({page}) => {
    await devSet(page, 'encryption=0');
    await page.goto('./');
    await expect(page.getByTestId('banner-encryption')).toContainText('encrypted only');
    await expect(page.getByTestId('banner-encryption').locator('a')).toHaveAttribute('href', '/backup#encryption');
    await devSet(page, 'encryption=1&provider=0');
    await page.goto('./');
    await expect(page.getByTestId('banner-provider')).toContainText('2.0.0-beta.7');
    await devSet(page, 'provider=1');
});

test('connect a WebDAV account: a wrong password says so, the right one makes the card', async ({page}) => {
    await page.goto('./');
    const dlg = await connectWebDAV(page, 'NAS', 'wrong');
    const box = await dlg.boundingBox();
    await dlg.getByTestId('save').click();
    await expect(dlg.getByTestId('connect-error')).toContainText(/401|Unauthorized/i);
    // the dialog keeps its box whether its error line says something or not
    expect(await dlg.boundingBox()).toEqual(box);
    await expect(page.getByTestId('account')).toHaveCount(0);
    await dlg.getByTestId('password').fill('dev-pass');
    await dlg.getByTestId('save').click();
    await expect(dlg).toHaveCount(0);
    const card = account(page, 'NAS');
    await expect(card).toContainText('WebDAV · backup@127.0.0.1:18293');
    await expect(card.getByTestId('pill')).toHaveAttribute('data-state', 'writable');
    await expect(card.getByTestId('unused')).toBeVisible();
    await expect(card.getByTestId('relogin')).toHaveText('Change the credentials');
    const id = await card.getAttribute('data-account');
    await expect(card.getByTestId('use-for-backups')).toHaveAttribute('href', `/backup?add-target=sync&destination=${id}#targets`);
    await expect(card.getByTestId('use-for-backups')).toHaveAttribute('target', '_top');
    await expect(page.getByTestId('empty')).toHaveCount(0);
    // no secret comes back
    const state = await (await page.request.get('api/v1/state')).text();
    expect(state).not.toContain('dev-pass');
    // the test
    await card.getByTestId('test').click();
    await expect(page.getByText('NAS answers.')).toBeVisible();
});

test('edit: the name and the upload limit', async ({page}) => {
    await page.goto('./');
    const card = account(page, 'NAS');
    await card.getByTestId('edit').click();
    const dlg = page.getByTestId('edit-dialog');
    await dlg.getByTestId('edit-name').fill('Keller-NAS');
    await dlg.getByTestId('edit-limit').fill('512');
    await dlg.getByTestId('edit-save').click();
    await expect(dlg).toHaveCount(0);
    await expect(account(page, 'Keller-NAS')).toContainText('Upload limit: 512 KiB/s');
});

for (const lang of ['en', 'de']) {
    test(`a pill and a button keep their box in every state (${lang})`, async ({page}) => {
        const states = ['writable', 'unreachable', 'auth-failed', 'full', 'read-only', 'error', 'something-new'];
        let current = states[0];
        let kind = 'webdav';
        await page.route('**/api/v1/state*', async (route) => {
            const res = await route.fetch();
            const body = await res.json();
            for (const a of body.accounts) {
                a.state = current;
                a.kind = kind;
                a.text = current === 'writable' ? '' : 'a line from rclone that is quite long: couldn\'t list files: 507 Insufficient Storage on https://cloud.example.org/remote.php/dav/files/someone';
            }
            await route.fulfill({json: body});
        });
        if (lang === 'de') await german(page);
        await page.goto('./');
        const card = page.getByTestId('account').first();
        const pill = card.getByTestId('pill');
        await expect(pill).toHaveAttribute('data-state', 'writable');
        const first = await pill.boundingBox();
        const testButton = await card.getByTestId('test').boundingBox();
        const seen: string[] = [];
        for (const s of states) {
            current = s;
            await page.reload();
            await expect(pill).toHaveAttribute('data-state', s === 'something-new' ? 'error' : s);
            const box = await pill.boundingBox();
            expect(box?.width, `the pill in state ${s}`).toBe(first?.width);
            expect(box?.height).toBe(first?.height);
            expect((await card.getByTestId('test').boundingBox())?.width).toBe(testButton?.width);
            seen.push((await pill.locator('span').first().textContent()) ?? '');
        }
        expect(new Set(seen).size).toBe(states.length - 1);
        // an OAuth account's refused login is a lapsed one, and its button says "log in again"
        current = 'auth-failed';
        kind = 'onedrive';
        await page.reload();
        await expect(pill).toHaveAttribute('data-state', 'login-expired');
        expect((await pill.boundingBox())?.width).toBe(first?.width);
        await expect(card.getByTestId('relogin')).toHaveText(lang === 'de' ? 'Erneut anmelden' : 'Log in again');
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    });
}

test('an account a backup target uses: the card says so, and removing it names the target', async ({page}) => {
    await page.goto('./');
    const card = account(page, 'Keller-NAS');
    const id = await card.getAttribute('data-account');
    await devSet(page, `used=${id}&name=OneDrive`);
    await page.goto('./');
    await expect(card.getByTestId('used-by')).toContainText('Used by the backup target "OneDrive".');
    await expect(card.getByTestId('used-by').locator('a')).toHaveAttribute('href', '/backup#targets');
    await expect(card.getByTestId('use-for-backups')).toHaveCount(0);
    await card.getByTestId('remove').click();
    const dlg = page.getByTestId('remove-dialog');
    await expect(dlg).toContainText('Remove the account "Keller-NAS"?');
    await dlg.getByTestId('remove-dialog-yes').click();
    // the system says a target uses it: the question names the target and asks again
    await expect(dlg).toContainText('The backup target "OneDrive" uses this account.');
    await expect(dlg.getByTestId('remove-dialog-yes')).toHaveText('Remove anyway');
    await dlg.getByRole('button', {name: 'Cancel'}).click();
    await expect(account(page, 'Keller-NAS')).toHaveCount(1);
});

test('a login at a cloud: the three steps, the address pasted; and rclone on another computer', async ({page, context}) => {
    await page.goto('./');
    await page.getByTestId('connect').click();
    const dlg = page.getByTestId('connect-dialog');
    const box = await dlg.boundingBox();
    await dlg.getByTestId('kind').selectOption('dropbox');
    await dlg.getByTestId('name').fill('Privat');
    await expect(dlg.getByTestId('privacy')).toContainText('Dropbox receives the backups of this system. They are encrypted before they leave it');
    await expect(dlg.getByTestId('steps').locator('li')).toHaveCount(3);
    await expect(dlg.getByTestId('address')).toBeDisabled();
    await expect(dlg.getByTestId('finish')).toBeDisabled();
    expect(await dlg.boundingBox()).toEqual(box);
    // step 1 opens the provider's login in a new tab; the stand-in sends it straight to rclone's address
    const [tab] = await Promise.all([context.waitForEvent('page'), dlg.getByTestId('open-login').click()]);
    await tab.waitForURL(/^http:\/\/localhost:53682\/\?/);
    const address = tab.url();
    expect(address).toMatch(/state=[\w-]+&code=FAKE-CODE|code=FAKE-CODE&state=/);
    await tab.close();
    await expect(dlg.getByTestId('address')).toBeEnabled();
    await expect(dlg.getByTestId('open-login')).toHaveText('Open the login again');
    await expect(dlg.getByTestId('kind')).toBeDisabled();
    // an address that is not the login's end does not enable Finish
    await dlg.getByTestId('address').fill('https://www.dropbox.com/home');
    await expect(dlg.getByTestId('finish')).toBeDisabled();
    await dlg.getByTestId('address').fill(address);
    await expect(dlg.getByTestId('finish')).toBeEnabled();
    // the last step contacts the real cloud with the token: answered here
    let sent: any;
    await page.route('**/api/v1/login/finish', async (route) => {
        sent = route.request().postDataJSON();
        await route.fulfill({status: 502, json: {error: 'account', state: 'auth-failed', message: 'Dropbox refuses the token (a stand-in login)'}});
    });
    await dlg.getByTestId('finish').click();
    await expect(dlg.getByTestId('connect-error')).toHaveText('Dropbox refuses the token (a stand-in login)');
    expect(sent.address).toBe(address);
    expect(sent.id).toBeTruthy();
    expect(await dlg.boundingBox()).toEqual(box);

    // the other way: rclone's own command, and what it prints
    await dlg.getByTestId('way-rclone').click();
    await expect(dlg.getByTestId('command')).toHaveText('rclone authorize "dropbox"');
    await expect(dlg.getByTestId('finish')).toBeDisabled();
    await dlg.getByTestId('token').fill('eyJ0b2tlbiI6IntcImFjY2Vzc190b2tlblwiOlwiQVRcIn0ifQ');
    await dlg.getByTestId('finish').click();
    await expect.poll(() => sent.token).toBe('eyJ0b2tlbiI6IntcImFjY2Vzc190b2tlblwiOlwiQVRcIn0ifQ');
    expect(sent.spec).toMatchObject({kind: 'dropbox', name: 'Privat'});
    expect(await dlg.boundingBox()).toEqual(box);
    await dlg.getByRole('button', {name: 'Cancel'}).click();
    await expect(dlg).toHaveCount(0);
    // the cancelled login is gone on the system
    const state = await (await page.request.get('api/v1/state')).json();
    expect(state.login).toBeUndefined();
});

test('Google Drive asks for an own client, or a service account with its folder', async ({page}) => {
    await page.goto('./');
    await page.getByTestId('connect').click();
    const dlg = page.getByTestId('connect-dialog');
    await dlg.getByTestId('kind').selectOption('drive');
    await dlg.getByTestId('name').fill('Haus');
    await expect(dlg.getByTestId('drive-note')).toContainText('client ID of your own');
    await expect(dlg.getByTestId('open-login')).toBeDisabled();
    await dlg.getByTestId('client-id').fill('1234.apps.googleusercontent.com');
    await dlg.getByTestId('client-secret').fill('GOCSPX-secret');
    await expect(dlg.getByTestId('open-login')).toBeEnabled();
    await dlg.getByTestId('way-rclone').click();
    await expect(dlg.getByTestId('command')).toHaveText('rclone authorize "drive" "1234.apps.googleusercontent.com" "GOCSPX-secret"');
    await dlg.getByTestId('way-browser').click();
    await dlg.getByTestId('service-account').check();
    await expect(dlg.getByTestId('steps')).toHaveCount(0);
    await expect(dlg.getByTestId('save')).toBeDisabled();
    await dlg.getByTestId('service-account-key').fill('{"type": "service_account"}');
    await dlg.getByTestId('folder-id').fill('1AbCdEfGhIjK');
    await expect(dlg.getByTestId('save')).toBeEnabled();
    await dlg.getByTestId('save').click();
    await expect(dlg.getByTestId('connect-error')).toContainText("service account's key");
    await dlg.getByRole('button', {name: 'Cancel'}).click();
});

test('new credentials keep the account; removing it forgets it and leaves the files', async ({page}) => {
    await devSet(page, 'unused=1');
    await page.goto('./');
    const card = account(page, 'Keller-NAS');
    const id = await card.getAttribute('data-account');
    await card.getByTestId('relogin').click();
    const dlg = page.getByTestId('connect-dialog');
    await expect(dlg.getByTestId('kind')).toHaveCount(0);
    await dlg.getByTestId('url').fill(DAV);
    await dlg.getByTestId('vendor').selectOption('other');
    await dlg.getByTestId('user').fill('backup');
    await dlg.getByTestId('password').fill('nope');
    await dlg.getByTestId('save').click();
    await expect(dlg.getByTestId('connect-error')).not.toHaveText('');
    await dlg.getByTestId('password').fill('dev-pass');
    await dlg.getByTestId('save').click();
    await expect(dlg).toHaveCount(0);
    await expect(account(page, 'Keller-NAS')).toHaveAttribute('data-account', id!);
    await account(page, 'Keller-NAS').getByTestId('remove').click();
    await page.getByTestId('remove-dialog-yes').click();
    await expect(page.getByTestId('account')).toHaveCount(0);
    await expect(page.getByText('Keller-NAS was removed.')).toBeVisible();
    await expect(page.getByTestId('empty')).toBeVisible();
});

test('a phone: nothing is wider than the screen, in the page and in the dialog', async ({page}) => {
    await page.setViewportSize({width: 360, height: 740});
    await german(page);
    await page.goto('./');
    await (await connectWebDAV(page, 'Ein ziemlich langer Name für ein Konto auf einem NAS im Keller')).getByTestId('save').click();
    await expect(page.getByTestId('account')).toHaveCount(1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.getByTestId('connect').click();
    await page.getByTestId('connect-dialog').getByTestId('kind').selectOption('drive');
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.getByTestId('connect-dialog').getByRole('button', {name: 'Abbrechen'}).click();
    await page.getByTestId('account').getByTestId('remove').click();
    await page.getByTestId('remove-dialog-yes').click();
    await expect(page.getByTestId('account')).toHaveCount(0);
});
