import {describe, expect, it} from 'vitest';

import {cleanSpec, fieldsOf, formatBytes, looksLikeRedirect, pillOf, specReady, useForBackupsHref, PILLS} from './model.js';

const OAUTH = ['onedrive', 'drive', 'dropbox'];

describe('pills', () => {
    it('a lapsed OAuth login is told apart from refused keys', () => {
        expect(pillOf({state: 'auth-failed', kind: 'onedrive'}, OAUTH)).toBe('login-expired');
        expect(pillOf({state: 'auth-failed', kind: 'webdav'}, OAUTH)).toBe('auth-failed');
        expect(pillOf({state: 'auth-failed', kind: 'drive', auth: 'service-account'}, OAUTH)).toBe('auth-failed');
    });
    it('an unknown state is an error, and every pill has a label', () => {
        expect(pillOf({state: 'splendid', kind: 's3'}, OAUTH)).toBe('error');
        for (const s of ['writable', 'unreachable', 'auth-failed', 'full', 'read-only', 'error']) expect(PILLS[pillOf({state: s, kind: 's3'}, OAUTH)]).toBeTruthy();
    });
});

describe('forms', () => {
    it('Google Drive needs an own client, or a service account with its folder', () => {
        expect(specReady({name: 'G', kind: 'drive'}, false)).toBe(false);
        expect(specReady({name: 'G', kind: 'drive', client_id: 'a', client_secret: 'b'}, false)).toBe(true);
        expect(specReady({name: 'G', kind: 'drive', service_account: '{}'}, true)).toBe(false);
        expect(specReady({name: 'G', kind: 'drive', service_account: '{}', folder_id: 'abcdef'}, true)).toBe(true);
    });
    it('OneDrive and Dropbox work without a client, and with one only as a pair', () => {
        expect(specReady({name: 'O', kind: 'onedrive'}, false)).toBe(true);
        expect(specReady({name: 'O', kind: 'onedrive', client_id: 'a'}, false)).toBe(false);
        expect(specReady({name: '', kind: 'dropbox'}, false)).toBe(false);
    });
    it('S3 needs an endpoint unless it is AWS; WebDAV an address with a scheme', () => {
        const s3 = {name: 'B', kind: 's3', bucket: 'b', access_key: 'k', secret_key: 's'};
        expect(specReady({...s3, provider: 'Other'}, false)).toBe(false);
        expect(specReady({...s3, provider: 'AWS'}, false)).toBe(true);
        expect(specReady({...s3, provider: 'Other', endpoint: 'https://s3.example.org'}, false)).toBe(true);
        expect(specReady({name: 'W', kind: 'webdav', url: 'cloud.example.org', user: 'u', password: 'p'}, false)).toBe(false);
        expect(specReady({name: 'W', kind: 'webdav', url: 'https://cloud.example.org/dav', user: 'u', password: 'p'}, false)).toBe(true);
    });
    it('only the fields of the kind are sent, a password as typed', () => {
        const s = cleanSpec({name: ' NAS ', kind: 'webdav', url: ' https://x.example/ ', user: 'u', password: ' p ', bucket: 'left over', client_id: 'x'}, false);
        expect(s).toEqual({name: 'NAS', kind: 'webdav', url: 'https://x.example/', user: 'u', password: ' p '});
        expect(fieldsOf('drive', true)).toEqual(['service_account', 'folder_id']);
    });
});

describe('helpers', () => {
    it('the address a login ends on', () => {
        expect(looksLikeRedirect('http://localhost:53682/?state=abc&code=xyz')).toBe(true);
        expect(looksLikeRedirect(' http://127.0.0.1:53682/?code=x&state=y ')).toBe(true);
        expect(looksLikeRedirect('http://localhost:53682/?error=access_denied&state=y')).toBe(true);
        expect(looksLikeRedirect('https://login.example.org/?code=x')).toBe(false);
        expect(looksLikeRedirect('http://localhost:53682/')).toBe(false);
    });
    it('sizes and the hand-over to the Backup page', () => {
        expect(formatBytes(0)).toBe('');
        expect(formatBytes(8_400_000)).toBe('8 MB');
        expect(formatBytes(4_100_000_000)).toBe('4.1 GB');
        expect(useForBackupsHref('a1b2')).toBe('/backup?add-target=sync&destination=a1b2#targets');
    });
});
