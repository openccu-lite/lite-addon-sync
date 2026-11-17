// The page's model: the API's shapes, what a state means on a pill, and the request a form makes.

export interface Target { id: string; name: string }
export interface Upload { at: string; ok: boolean; name?: string; size?: number }
export interface Account {
    id: string; name: string; kind: string; detail: string; root?: string; auth?: string; own_client?: boolean; created: string;
    state: string; text?: string; checked?: string; upload_limit_kbps: number; last_upload?: Upload; free_bytes?: number; total_bytes?: number;
    used_by: Target[]; jobs?: string[];
}
export interface Kind { id: string; label: string; oauth: boolean }
export interface Login { id: string; url: string; expires: string }
export interface SystemFacts { reachable: boolean; provider: boolean; encryption: boolean; targets: Record<string, Target[]>; error?: string }
export interface State { version: string; rclone: string; kinds: Kind[]; accounts: Account[]; login?: Login; system: SystemFacts; provider: boolean; provider_problem?: string; now: number; servers: Server[]; copy: CopyView }

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

// ---- copy jobs -----------------------------------------------------------------------------------

export interface HostKey { type: string; fingerprint: string }
export interface Server {
    id: string; name: string; host: string; port: number; user: string; path: string; created: string; public_key: string;
    host_keys: HostKey[]; pending?: HostKey[]; state: string; text?: string; checked?: string; jobs: string[];
}
export interface Schedule { kind: string; time?: string; weekday?: string; calendar?: string[] }
export interface JobTarget { kind: 'account' | 'server' | string; id: string; folder: string }
export interface Run {
    id: string; job: string; job_name: string; start: string; end: string; trigger: string; dry_run: boolean; tool: string; result: string;
    copied: number; replaced: number; deleted: number; errors: number; bytes: number; exit: number; error?: string; log_cut?: boolean;
}
export interface Progress { run: string; started: string; dry_run: boolean; waiting?: boolean; files: number; bytes: number; percent: number; speed?: string; eta?: string }
export interface Job {
    id: string; name: string; source: string; read_addon?: string; target: JobTarget; mode: string; max_delete: number; excludes: string[];
    name_filters: boolean; bwlimit_kbps: number; versions?: boolean; versions_days?: number; encrypt?: boolean; schedule: Schedule; needs_dry_run?: boolean;
    timer: {unit: string; exists: boolean; enabled: boolean; next?: string; last?: string};
    target_name: string; target_gone?: boolean; last?: Run; running?: Progress;
}
export interface CopyView { supported: boolean; problem?: string; readable: string[]; jobs: Job[]; name_filters: string[]; fixed: string[]; root: string }
export interface Folder { name: string; path: string; state: string; why?: string; addon?: string; open: boolean }
export interface Listing { path: string; parent: string; self: Folder; folders: Folder[] }
export interface JobSpec {
    name: string; source: string; read_addon: string; target: JobTarget; mode: string; max_delete: number; excludes: string[];
    name_filters: boolean; bwlimit_kbps: number; versions: boolean; versions_days: number; encrypt: boolean; schedule: Schedule; replace_custom?: boolean;
}

/** a server's pill by state; every pill reserves the widest */
export const SERVER_PILLS: Record<string, string> = {
    'not-tested': 'not tested',
    ok: 'ok',
    unreachable: 'unreachable',
    'auth-failed': 'login refused',
    'hostkey-unknown': 'host key unknown',
    'hostkey-changed': 'host key changed',
    'no-rsync': 'no rsync on the server',
    'read-only': 'no write access',
    error: 'error',
    testing: 'testing…',
};
export const serverPillClass = (state: string) => (state === 'ok' ? 'ok' : state === 'not-tested' || state === 'testing' ? '' : 'bad');

/** a run's pill by result */
export const RUN_PILLS: Record<string, string> = {
    ok: 'ok',
    warnings: 'ok with warnings',
    failed: 'failed',
    stopped: 'stopped',
    'too-many-deletions': 'too many deletions',
    'target-unreachable': 'target unreachable',
    'login-expired': 'login expired',
    'host-key-changed': 'host key changed',
    'quota-full': 'storage full',
    'source-missing': 'source missing',
    'target-missing': 'target missing',
    busy: 'another job ran',
    'dry-run-first': 'try it first',
    never: 'never',
    running: 'running…',
    waiting: 'waiting…',
};
export const runPillClass = (result: string) => (result === 'ok' ? 'ok' : result === 'warnings' || result === 'never' || result === 'running' || result === 'waiting' || result === 'stopped' ? '' : 'bad');

export const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

/** the schedule column's words: a key of the catalogue and its values */
export function scheduleWords(s: Schedule): {key: string; values?: Record<string, string>} {
    switch (s.kind) {
        case 'hourly':
            return {key: 'hourly'};
        case 'daily':
            return {key: 'daily {time}', values: {time: s.time ?? ''}};
        case 'weekly':
            return {key: 'weekly {day} {time}', values: {day: s.weekday ?? '', time: s.time ?? ''}};
        case 'custom':
            return {key: 'custom'};
    }
    return {key: 'manual'};
}

/** the job dialog's start: a job's spec, or an empty one */
export function specOf(j?: Job): JobSpec {
    if (!j) {
        return {name: '', source: '', read_addon: '', target: {kind: 'account', id: '', folder: ''}, mode: 'copy', max_delete: 50, excludes: [], name_filters: true, bwlimit_kbps: 0, versions: false, versions_days: 30, encrypt: false, schedule: {kind: 'manual', time: '03:00', weekday: 'Mon'}};
    }
    return {
        name: j.name, source: j.source, read_addon: j.read_addon ?? '', target: {...j.target}, mode: j.mode, max_delete: j.max_delete, excludes: [...(j.excludes ?? [])],
        name_filters: j.name_filters, bwlimit_kbps: j.bwlimit_kbps, versions: !!j.versions, versions_days: j.versions ? (j.versions_days ?? 0) : 30, encrypt: !!j.encrypt, schedule: {kind: j.schedule.kind, time: j.schedule.time ?? '03:00', weekday: j.schedule.weekday ?? 'Mon', calendar: j.schedule.calendar},
    };
}

/** whether a job's form can be sent */
export function jobReady(s: JobSpec): boolean {
    if (!s.name.trim() || !s.source || !s.target.id || !s.target.folder.trim()) return false;
    if ((s.schedule.kind === 'daily' || s.schedule.kind === 'weekly') && !/^([01]\d|2[0-3]):[0-5]\d$/.test(s.schedule.time ?? '')) return false;
    return true;
}

/** the exclude patterns of a textarea: one per line, empty lines dropped */
export const patternsOf = (text: string) => text.split('\n').map((l) => l.trim()).filter((l) => l !== '');

export function duration(start: string, end: string): string {
    const s = Math.max(0, Math.round((new Date(end).getTime() - new Date(start).getTime()) / 1000));
    if (s >= 3600) return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`;
    if (s >= 60) return `${Math.floor(s / 60)} min ${s % 60} s`;
    return `${s} s`;
}

/** sizes down to bytes, for a run's figures */
export function formatSize(n: number): string {
    if (n >= 1e9) return `${(n / 1e9).toFixed(1)} GB`;
    if (n >= 1e6) return `${(n / 1e6).toFixed(1)} MB`;
    if (n >= 1e3) return `${Math.round(n / 1e3)} kB`;
    return `${n} B`;
}

/** the system's pages a job's row links: its timer on the Services page, its unit in the log */
export const timerHref = (j: Pick<Job, 'timer'>) => `/system/services?edit=${encodeURIComponent(j.timer.unit)}`;
export const logHref = (j: Pick<Job, 'timer'>) => `/system/log?unit=${encodeURIComponent(j.timer.unit.replace(/\.timer$/, ''))}`;
