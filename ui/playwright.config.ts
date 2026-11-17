import {defineConfig} from '@playwright/test';

// The page against the daemon, the fake system and a local WebDAV server (scripts/dev-serve.sh):
// build the page first (npm run build). Run as `env -u WAYLAND_DISPLAY -u DISPLAY npx playwright
// test` on WSL. One worker: the tests share the daemon's accounts and rclone's one login port.
export default defineConfig({
    testDir: 'e2e',
    timeout: 60_000,
    workers: 1,
    use: {baseURL: 'http://127.0.0.1:18292/addons/sync/', locale: 'en-GB'},
    webServer: {
        command: '../scripts/dev-serve.sh',
        url: 'http://127.0.0.1:18292/addons/sync/api/v1/health',
        reuseExistingServer: false,
        timeout: 120_000,
    },
});
