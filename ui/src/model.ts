// The page's model: the API's shapes, what a state means on a pill, and the request a form makes.

export interface Target { id: string; name: string }
export interface Upload { at: string; ok: boolean; name?: string; size?: number }
export interface Account {
    id: string; name: string; kind: string; detail: string; root?: string; auth?: string; own_client?: boolean; created: string;
    state: string; text?: string; checked?: string; upload_limit_kbps: number; last_upload?: Upload; free_bytes?: number; total_bytes?: number;
    used_by: Target[];
}
export interface Kind { id: string; label: string; oauth: boolean }
export interface Login { id: string; url: string; expires: string }
export interface SystemFacts { reachable: boolean; provider: boolean; encryption: boolean; targets: Record<string, Target[]>; error?: string }
export interface State { version: string; rclone: string; kinds: Kind[]; accounts: Account[]; login?: Login; system: SystemFacts; provider: boolean; provider_problem?: string; now: number }

/** what the user enters for an account; the keys are the API's */
export interface Spec {
    name: string; kind: string;
    client_id?: string; client_secret?: string; service_account?: string; folder_id?: string;
    provider?: string; endpoint?: string; region?: string; bucket?: string; access_key?: string; secret_key?: string;
    url?: string; vendor?: string; user?: string; password?: string;
}

/** the pills' labels by state, as English keys of the catalogue; every pill reserves the widest */
export const PILLS: Record<string, string> = {
    writable: 'ok',
    unreachable: 'unreachable',
    'auth-failed': 'login refused',
    'login-expired': 'login expired',
    full: 'storage full',
    'read-only': 'no write access',
    error: 'error',
    testing: 'testing…',
};

/** an account's pill: the state, with a lapsed OAuth login told apart from refused keys */
export function pillOf(a: Pick<Account, 'state' | 'kind' | 'auth'>, oauthKinds: string[]): string {
    if (a.state === 'auth-failed' && oauthKinds.includes(a.kind) && a.auth !== 'service-account') return 'login-expired';
    return a.state in PILLS ? a.state : 'error';
}

export const pillClass = (pill: string) => (pill === 'writable' ? 'ok' : pill === 'testing' ? '' : 'bad');

export function formatBytes(n?: number): string {
    if (!n || n <= 0) return '';
    if (n >= 1e12) return `${(n / 1e12).toFixed(1)} TB`;
    if (n >= 1e9) return `${(n / 1e9).toFixed(1)} GB`;
    return `${Math.max(1, Math.round(n / 1e6))} MB`;
}

/** the fields a kind's form shows, in order */
export function fieldsOf(kind: string, serviceAccount: boolean): string[] {
    switch (kind) {
        case 'onedrive':
        case 'dropbox':
            return ['client_id', 'client_secret'];
        case 'drive':
            return serviceAccount ? ['service_account', 'folder_id'] : ['client_id', 'client_secret'];
        case 's3':
            return ['provider', 'endpoint', 'region', 'bucket', 'access_key', 'secret_key'];
        case 'webdav':
            return ['url', 'vendor', 'user', 'password'];
    }
    return [];
}

/** whether a form can be sent: the name and what the kind cannot do without */
export function specReady(s: Spec, serviceAccount: boolean): boolean {
    if (!s.name.trim()) return false;
    const has = (k: keyof Spec) => !!(s[k] ?? '').toString().trim();
    switch (s.kind) {
        case 'onedrive':
        case 'dropbox':
            return has('client_id') === has('client_secret');
        case 'drive':
            return serviceAccount ? has('service_account') && has('folder_id') : has('client_id') && has('client_secret');
        case 's3':
            return has('bucket') && has('access_key') && has('secret_key') && (s.provider === 'AWS' || has('endpoint'));
        case 'webdav':
            return /^https?:\/\/[^/\s]+/.test(s.url ?? '') && has('user') && has('password');
    }
    return false;
}

/** the spec as it is sent: trimmed, and only the fields of its kind */
export function cleanSpec(s: Spec, serviceAccount: boolean): Spec {
    const out: Spec = {name: s.name.trim(), kind: s.kind};
    for (const k of fieldsOf(s.kind, serviceAccount) as (keyof Spec)[]) {
        const v = (s[k] ?? '').toString();
        const t = k === 'password' || k === 'secret_key' || k === 'service_account' ? v : v.trim();
        if (t) (out as unknown as Record<string, string>)[k] = t;
    }
    return out;
}

/** whether a pasted text looks like the address a login ends on */
export const looksLikeRedirect = (s: string) => /^http:\/\/(localhost|127\.0\.0\.1):53682\/\?\S*\bcode=/.test(s.trim()) || /^http:\/\/(localhost|127\.0\.0\.1):53682\/\?\S*\berror=/.test(s.trim());

/** the system's Backup page with the new-target form open for an account */
export const useForBackupsHref = (id: string) => `/backup?add-target=sync&destination=${encodeURIComponent(id)}#targets`;

/** which company receives the backups of a kind (the notice before a login) */
export const COMPANY: Record<string, string> = {onedrive: 'Microsoft', drive: 'Google', dropbox: 'Dropbox'};
