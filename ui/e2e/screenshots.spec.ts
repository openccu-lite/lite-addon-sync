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
        if (scheme === 'dark') {
            await page.request.get(`${FAKE}/dev/set?unused=1`);
            await page.goto('./');
            await page.getByTestId('remove').click();
            await page.getByTestId('remove-dialog-yes').click();
            await expect(page.getByTestId('account')).toHaveCount(0);
        }
    });
}
