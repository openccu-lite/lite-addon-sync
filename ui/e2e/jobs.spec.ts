import {expect, test, type Page} from '@playwright/test';

// Copy jobs against the daemon, the real rclone, a WebDAV server on this machine and the fake
// system, whose timers are kept in memory and whose "run" starts the addon's own runner
// (scripts/dev-serve.sh). The tests run in order and leave the page as they found it: the other
// specs expect no account.

const FAKE = 'http://127.0.0.1:18081';
const DAV = 'http://127.0.0.1:18293/';

async function devSet(page: Page, query: string) {
    const res = await page.request.get(`${FAKE}/dev/set?${query}`);
    expect(res.ok()).toBe(true);
}
async function timer(page: Page, name: string) {
    const res = await page.request.get(`${FAKE}/api/system/v1/addon-timers`, {headers: {Authorization: 'Bearer dev'}});
    const body = (await res.json()) as {timers: {name: string; timer_file: string; enabled: boolean; description: string}[]};
    return body.timers.find((t) => t.name === name);
}
const german = (page: Page) => page.addInitScript(() => localStorage.setItem('sync.language', 'de'));
const row = (page: Page, name: string) => page.getByTestId('job').filter({hasText: name});
const box = async (page: Page, sel: ReturnType<Page['locator']>) => {
    const b = await sel.boundingBox();
    return b ? {w: Math.round(b.width), h: Math.round(b.height)} : null;
};

test.describe.configure({mode: 'serial'});

test('the tabs, and a Copy jobs tab that says what it needs', async ({page}) => {
    await devSet(page, 'encryption=1&provider=1&unused=1&timers=1');
    await page.goto('./#jobs');
    await expect(page.getByRole('tab', {name: 'Copy jobs'})).toHaveAttribute('aria-selected', 'true');
    await expect(page.getByTestId('jobs-notarget')).toContainText('connect an account or add an SSH server first');
    await expect(page.getByTestId('job-add')).toBeDisabled();
    // an older system: said, and nothing offered
    await devSet(page, 'timers=0');
    await page.reload();
    await expect(page.getByTestId('banner-timers')).toContainText('copy jobs need openccu-lite 2.0.0-beta.7 or newer');
    await devSet(page, 'timers=1');
    await page.getByRole('tab', {name: 'History'}).click();
    await expect(page.getByTestId('history-empty')).toBeVisible();
    await expect(page).toHaveURL(/#history$/);
    await page.getByRole('tab', {name: 'Accounts'}).click();
    await expect(page.getByRole('heading', {name: 'SSH servers'})).toBeVisible();
});

test('an SSH server: a key of its own, the host key read before it is trusted, removed again', async ({page}) => {
    await page.goto('./');
    await page.getByTestId('server-add').click();
    const dlg = page.getByTestId('server-dialog');
    await expect(dlg.getByTestId('server-save')).toBeDisabled();
    await dlg.getByTestId('server-name').fill('NAS');
    await dlg.getByTestId('server-host').fill('-oProxyCommand=id');
    await dlg.getByTestId('server-user').fill('backup');
    await dlg.getByTestId('server-path').fill('/volume1/ccu');
    await dlg.getByTestId('server-save').click();
    await expect(dlg.getByTestId('server-error')).toContainText('host');
    await dlg.getByTestId('server-host').fill('127.0.0.1');
    await dlg.getByTestId('server-port').fill('9');
    await dlg.getByTestId('server-save').click();
    // the three steps: the key's line, the host key, the test
    await expect(dlg.getByTestId('server-pubkey')).toContainText(/^restrict ssh-ed25519 AAAA/);
    await expect(dlg.getByTestId('server-dialog-test')).toBeDisabled();
    const pill = dlg.getByTestId('server-dialog-pill');
    const before = await box(page, pill);
    await dlg.getByTestId('server-scan').click();
    // nothing listens there: no key is shown, nothing can be trusted
    await expect(pill).toHaveAttribute('data-state', 'unreachable', {timeout: 30_000});
    await expect(dlg.getByTestId('server-trust')).toHaveCount(0);
    expect(await box(page, pill)).toEqual(before);
    await dlg.getByTestId('server-done').click();
    const card = page.getByTestId('server').filter({hasText: 'NAS'});
    await expect(card.getByTestId('server-nohostkey')).toBeVisible();
    await expect(card).toContainText('backup@127.0.0.1:9');
    await card.getByTestId('server-remove').click();
    await page.getByTestId('server-remove-dialog-yes').click();
    await expect(page.getByTestId('server')).toHaveCount(0);
});

test('a copy job: the folder picker and what is never copied, tried, run, in the history', async ({page}) => {
    await page.goto('./');
    await page.getByTestId('connect').click();
    const cd = page.getByTestId('connect-dialog');
    await cd.getByTestId('kind').selectOption('webdav');
    await cd.getByTestId('name').fill('Kopien');
    await cd.getByTestId('url').fill(DAV);
    await cd.getByTestId('vendor').selectOption('other');
    await cd.getByTestId('user').fill('backup');
    await cd.getByTestId('password').fill('dev-pass');
    await cd.getByTestId('save').click();
    await expect(page.getByTestId('account').filter({hasText: 'Kopien'})).toBeVisible();

    await page.getByRole('tab', {name: 'Copy jobs'}).click();
    await expect(page.getByTestId('jobs-empty')).toContainText('No copy job yet.');
    await page.getByTestId('job-add').click();
    const dlg = page.getByTestId('job-dialog');
    await expect(dlg.getByTestId('job-save')).toBeDisabled();
    await dlg.getByTestId('job-name').fill('Export');
    await dlg.getByTestId('job-pick').click();
    const picker = dlg.getByTestId('picker');
    const folder = (name: string) => picker.locator(`[data-testid="picker-folder"][data-name="${name}"]`);
    // the storage's top: the settings and the temporary files are struck out and cannot be chosen
    await expect(folder('etc')).toHaveAttribute('data-state', 'never');
    await expect(folder('etc')).toContainText('never copied');
    await expect(folder('etc').getByTestId('picker-choose')).toHaveCount(0);
    await expect(folder('tmp')).toHaveAttribute('data-state', 'never');
    await expect(dlg.getByTestId('picker-choose-this')).toBeDisabled();
    await picker.getByTestId('picker-never').click();
    await expect(picker.getByTestId('picker-never-list')).toContainText('*.sbk');
    // a folder of backups and a folder of program files, one level down
    await folder('sdcard').getByRole('button', {name: 'sdcard/'}).click();
    await expect(folder('backup')).toHaveAttribute('data-state', 'never');
    await expect(folder('backup')).toContainText('Backup page');
    await picker.getByTestId('picker-up').click();
    await folder('addons').getByRole('button', {name: 'addons/'}).click();
    await folder('tools').getByRole('button', {name: 'tools/'}).click();
    await expect(folder('bin')).toContainText('program files');
    await picker.getByTestId('picker-up').click();
    await picker.getByTestId('picker-up').click();
    await folder('export').getByTestId('picker-choose').click();
    await expect(dlg.getByTestId('job-source')).toHaveText('export');
    await dlg.getByTestId('job-target').selectOption({index: 1});
    await dlg.getByTestId('job-folder').fill('--config=/etc');
    await dlg.getByTestId('job-save').click();
    await expect(dlg.getByTestId('job-error')).toContainText('folder');
    await dlg.getByTestId('job-folder').fill('kopien/export');
    await dlg.getByTestId('job-schedule-kind').selectOption('daily');
    await dlg.getByTestId('job-time').fill('03:00');
    // versions: off by default, 30 days when switched on
    await expect(dlg.getByTestId('job-versions-days')).toHaveCount(0);
    await dlg.getByTestId('job-versions').check();
    await expect(dlg.getByTestId('job-versions-days')).toHaveValue('30');
    const created = page.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith('/api/v1/jobs'));
    await dlg.getByTestId('job-save').click();
    expect((await created).postDataJSON()).toMatchObject({versions: true, versions_days: 30});
    await expect(dlg).toBeHidden();

    const job = row(page, 'Export');
    await expect(job).toContainText('versions');
    await expect(job.getByTestId('job-schedule')).toHaveText('daily 03:00');
    await expect(job.getByTestId('job-pill')).toHaveAttribute('data-state', 'never');
    await expect(job.getByTestId('job-next')).not.toHaveText('–');
    const tm = await timer(page, 'export');
    expect(tm?.enabled).toBe(true);
    expect(tm?.description).toBe('Export');
    expect(tm?.timer_file).toBe('[Unit]\nDescription=sync-export\n\n[Timer]\nOnCalendar=*-*-* 03:00:00\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n');
    await expect(page.getByTestId('job-timer-link')).toHaveAttribute('href', '/system/services?edit=local-sync-export.timer');
    await expect(job.getByTestId('job-log')).toHaveAttribute('href', '/system/log?unit=local-sync-export');

    // the pill and the buttons keep their box from "never" over "running" to "ok"
    const pill = job.getByTestId('job-pill');
    const pillBox = await box(page, pill);
    const rowBox = await box(page, job);
    // tried: nothing copied, the list says what a run would do - without the key file
    await job.getByTestId('job-try').click();
    await expect(job.getByTestId('job-result')).toContainText('tried', {timeout: 30_000});
    await expect(pill).toHaveAttribute('data-state', 'ok');
    expect(await box(page, pill)).toEqual(pillBox);
    expect(await box(page, job)).toEqual(rowBox);
    await job.getByTestId('job-result').click();
    const rd = page.getByTestId('run-dialog');
    await expect(rd.getByTestId('run-dry')).toContainText('A run would copy 2 files');
    await expect(rd.getByTestId('run-lines')).toContainText('a.txt');
    await expect(rd.getByTestId('run-lines')).toContainText('sub/b.txt');
    await expect(rd.getByTestId('run-lines')).not.toContainText('server.pem');
    await rd.getByTestId('run-close').click();
    // the run
    await job.getByTestId('job-run').click();
    await expect(job.getByTestId('job-result')).not.toContainText('tried', {timeout: 30_000});
    await job.getByTestId('job-result').click();
    await expect(rd.getByTestId('run-summary')).toContainText('2 copied, 0 replaced, 0 deleted, 0 errors');
    await rd.getByTestId('run-filter').selectOption('D');
    await expect(rd.getByTestId('run-lines')).toContainText('Nothing to list.');
    await rd.getByTestId('run-close').click();
    expect(await box(page, pill)).toEqual(pillBox);

    await page.getByRole('tab', {name: 'History'}).click();
    await expect(page.getByTestId('history-row')).toHaveCount(2);
    await expect(page.getByTestId('history-row').first()).toContainText('Export');
    await expect(page.getByTestId('history-row').last()).toContainText('tried');
    await page.getByTestId('history-open').first().click();
    await expect(rd.getByTestId('run-summary')).toBeVisible();
    await rd.getByTestId('run-close').click();
    // the account says a job uses it, and is not removed
    await page.getByRole('tab', {name: 'Accounts'}).click();
    const acc = page.getByTestId('account').filter({hasText: 'Kopien'});
    await expect(acc.getByTestId('used-by-jobs')).toContainText('"Export"');
    await acc.getByTestId('remove').click();
    await page.getByTestId('remove-dialog-yes').click();
    await expect(page.getByText('delete the job first (Export)')).toBeVisible();
    await expect(acc).toBeVisible();
});

test('the schedule: manual only switches the timer off, an edit on the Services page is left alone, replaced only after a yes', async ({page}) => {
    await page.goto('./#jobs');
    const job = row(page, 'Export');
    // a person edits the timer on the Services page into something the four choices do not say
    const custom = '[Unit]\nDescription=sync-export\n\n[Timer]\nOnCalendar=Mon..Fri *-*-* 02:30:00\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n';
    await devSet(page, 'timer=export&text=' + encodeURIComponent(custom));
    await page.reload();
    await expect(job.getByTestId('job-schedule')).toHaveText('custom');
    await job.getByTestId('job-edit').click();
    const dlg = page.getByTestId('job-dialog');
    await expect(dlg.getByTestId('job-schedule-kind')).toHaveValue('custom');
    await expect(dlg.getByTestId('job-calendar')).toContainText('OnCalendar=Mon..Fri *-*-* 02:30:00');
    await expect(dlg.getByTestId('job-timer-advanced')).toHaveAttribute('href', '/system/services?edit=local-sync-export.timer');
    // a save that leaves the schedule alone leaves the timer alone
    await dlg.getByTestId('job-name').fill('Export 2');
    await dlg.getByTestId('job-save').click();
    await expect(dlg).toBeHidden();
    expect((await timer(page, 'export'))?.timer_file).toBe(custom);
    expect((await timer(page, 'export'))?.description).toBe('Export 2');
    // choosing a schedule over it asks first; "no" keeps the timer
    await row(page, 'Export 2').getByTestId('job-edit').click();
    await dlg.getByTestId('job-schedule-kind').selectOption('hourly');
    await dlg.getByTestId('job-save').click();
    const ask = page.getByTestId('job-custom-dialog');
    await expect(ask).toContainText('OnCalendar=Mon..Fri *-*-* 02:30:00');
    await ask.getByRole('button', {name: 'Cancel'}).click();
    expect((await timer(page, 'export'))?.timer_file).toBe(custom);
    await dlg.getByTestId('job-save').click();
    await page.getByTestId('job-custom-dialog-yes').click();
    await expect(dlg).toBeHidden();
    await expect(row(page, 'Export 2').getByTestId('job-schedule')).toHaveText('hourly');
    expect((await timer(page, 'export'))?.timer_file).toContain('OnCalendar=hourly');
    // manual only: off, the files stay
    await row(page, 'Export 2').getByTestId('job-edit').click();
    await dlg.getByTestId('job-schedule-kind').selectOption('manual');
    await dlg.getByTestId('job-save').click();
    await expect(dlg).toBeHidden();
    await expect(row(page, 'Export 2').getByTestId('job-schedule')).toHaveText('manual');
    await expect(row(page, 'Export 2').getByTestId('job-next')).toHaveText('–');
    const tm = await timer(page, 'export');
    expect(tm?.enabled).toBe(false);
    expect(tm?.timer_file).toContain('OnCalendar=hourly');
});

test('a mirror job runs only after it was tried and looked at; German; deleted again', async ({page}) => {
    await page.goto('./#jobs');
    const job = row(page, 'Export 2');
    await job.getByTestId('job-edit').click();
    const dlg = page.getByTestId('job-dialog');
    await dlg.getByTestId('job-mode').selectOption('mirror');
    await expect(dlg.getByTestId('job-mirrornote')).toContainText('Mirror deletes on the target');
    await expect(dlg.getByTestId('job-maxdelete')).toHaveValue('50');
    await dlg.getByTestId('job-schedule-kind').selectOption('hourly');
    await dlg.getByTestId('job-save').click();
    await expect(dlg).toBeHidden();
    await expect(job.getByTestId('job-schedule')).toContainText('try it first');
    await expect(job.getByTestId('job-run')).toBeDisabled();
    expect((await timer(page, 'export'))?.enabled).toBe(false);
    await job.getByTestId('job-try').click();
    await expect(job.getByTestId('job-result')).toContainText('tried', {timeout: 30_000});
    await job.getByTestId('job-result').click();
    const rd = page.getByTestId('run-dialog');
    await expect(rd.getByTestId('run-released')).toContainText('it may run now');
    await rd.getByTestId('run-close').click();
    await expect(job.getByTestId('job-run')).toBeEnabled();
    await expect(job.getByTestId('job-schedule')).toHaveText('hourly');
    expect((await timer(page, 'export'))?.enabled).toBe(true);

    // in German, with the same boxes
    const pillEn = await box(page, job.getByTestId('job-pill'));
    await german(page);
    await page.reload();
    await expect(page.getByRole('tab', {name: 'Kopieraufträge'})).toHaveAttribute('aria-selected', 'true');
    const de = row(page, 'Export 2');
    await expect(de.getByTestId('job-schedule')).toHaveText('stündlich');
    await expect(de.getByTestId('job-run')).toContainText('Jetzt ausführen');
    await expect(de.getByTestId('job-try')).toHaveText('Ausprobieren');
    expect((await box(page, de.getByTestId('job-pill')))?.h).toBe(pillEn?.h);
    // 360 px: the table scrolls inside its frame, the page does not
    await page.setViewportSize({width: 360, height: 740});
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.setViewportSize({width: 1280, height: 720});
    await page.evaluate(() => localStorage.setItem('sync.language', 'en'));
});

test('cleaning up: the job goes with its timer, the account can be removed again', async ({page}) => {
    await page.goto('./#jobs');
    await row(page, 'Export 2').getByTestId('job-delete').click();
    await expect(page.getByTestId('job-delete-dialog')).toContainText('local-sync-export.timer');
    await page.getByTestId('job-delete-dialog-yes').click();
    await expect(page.getByTestId('job')).toHaveCount(0);
    expect(await timer(page, 'export')).toBeUndefined();
    await page.getByRole('tab', {name: 'History'}).click();
    await expect(page.getByTestId('history-empty')).toBeVisible();
    await page.getByRole('tab', {name: 'Accounts'}).click();
    const acc = page.getByTestId('account').filter({hasText: 'Kopien'});
    await acc.getByTestId('remove').click();
    await page.getByTestId('remove-dialog-yes').click();
    await expect(page.getByTestId('account')).toHaveCount(0);
});
