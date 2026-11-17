import {expect, test} from '@playwright/test';

// The readme's screenshots (SCREENSHOTS=1 npx playwright test screenshots): German, light and dark,
// against the daemon of scripts/dev-serve.sh with its WebDAV server as the one account.
test.skip(!process.env.SCREENSHOTS, 'only on demand');
test.use({locale: 'de-DE', viewport: {width: 1100, height: 760}});

const FAKE = 'http://127.0.0.1:18081';

for (const scheme of ['light', 'dark'] as const) {
    test(`screenshots ${scheme}`, async ({page}) => {
        await page.emulateMedia({colorScheme: scheme});
        await page.addInitScript(() => localStorage.setItem('sync.language', 'de'));
        const suffix = scheme === 'dark' ? '-dunkel' : '';
        const out = (n: string) => `../docs/screenshots/${n}${suffix}.png`;
        await page.request.get(`${FAKE}/dev/set?encryption=1&provider=1&unused=1`);
        await page.goto('./');
        // the one account, connected once
        if (!(await page.getByTestId('account').count())) {
            await page.getByTestId('connect').click();
            const dlg = page.getByTestId('connect-dialog');
            await dlg.getByTestId('kind').selectOption('webdav');
            await dlg.getByTestId('name').fill('Nextcloud');
            await dlg.getByTestId('url').fill('http://127.0.0.1:18293/');
            await dlg.getByTestId('vendor').selectOption('other');
            await dlg.getByTestId('user').fill('backup');
            await dlg.getByTestId('password').fill('dev-pass');
            await dlg.getByTestId('save').click();
            await expect(page.getByTestId('account')).toHaveCount(1);
        }
        const id = await page.getByTestId('account').getAttribute('data-account');
        await page.request.get(`${FAKE}/dev/set?used=${id}&name=Nextcloud`);
        await page.goto('./');
        await expect(page.getByTestId('used-by')).toBeVisible();
        await page.waitForTimeout(300);
        await page.screenshot({path: out('01-konten')});
        // the login at a cloud: the three steps
        await page.getByTestId('connect').click();
        const dlg = page.getByTestId('connect-dialog');
        await dlg.getByTestId('kind').selectOption('onedrive');
        await dlg.getByTestId('name').fill('OneDrive privat');
        await page.waitForTimeout(300);
        await page.screenshot({path: out('02-anmeldung')});
        await dlg.getByTestId('kind').selectOption('s3');
        await page.waitForTimeout(300);
        await page.screenshot({path: out('03-s3')});
        await dlg.getByRole('button', {name: 'Abbrechen'}).click();
        // the copy jobs: the tab with one job, its dialog, the folder picker
        await page.setViewportSize({width: 1280, height: 760});
        await page.getByRole('tab', {name: 'Kopieraufträge'}).click();
        if (!(await page.getByTestId('job').count())) {
            await page.getByTestId('job-add').click();
            const jd = page.getByTestId('job-dialog');
            await jd.getByTestId('job-name').fill('Exporte');
            await jd.getByTestId('job-pick').click();
            await jd.locator('[data-testid="picker-folder"][data-name="export"]').getByTestId('picker-choose').click();
            await jd.getByTestId('job-target').selectOption({index: 1});
            await jd.getByTestId('job-folder').fill('openccu-lite/exporte');
            await jd.getByTestId('job-schedule-kind').selectOption('daily');
            await jd.getByTestId('job-time').fill('03:00');
            await jd.getByTestId('job-save').click();
            await expect(jd).toBeHidden();
            await page.getByTestId('job-run').click();
            await expect(page.getByTestId('job-pill')).toHaveAttribute('data-state', 'ok', {timeout: 30_000});
        }
        await page.waitForTimeout(400);
        await page.screenshot({path: out('04-kopierauftraege')});
        await page.getByTestId('job-edit').click();
        const jd = page.getByTestId('job-dialog');
        await page.waitForTimeout(300);
        await page.screenshot({path: out('05-auftrag')});
        await jd.getByRole('button', {name: 'Abbrechen'}).click();
        await page.setViewportSize({width: 1100, height: 760});
        if (scheme === 'dark') {
            await page.getByTestId('job-delete').click();
            await page.getByTestId('job-delete-dialog-yes').click();
            await expect(page.getByTestId('job')).toHaveCount(0);
            await page.request.get(`${FAKE}/dev/set?unused=1`);
            await page.goto('./');
            await page.getByTestId('remove').click();
            await page.getByTestId('remove-dialog-yes').click();
            await expect(page.getByTestId('account')).toHaveCount(0);
        }
    });
}
