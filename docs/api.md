# The addon's API

Under `/addons/sync/api/v1/`, JSON. The page uses it; a program may too. Authentication is the system's: the shell's
session (administrators only, confirmed with the system through the kit) or an API token of the system; a request
that changes something carries the header `X-CCU-Addon-Request`. The addon has no token and no port of its own.

**No answer ever holds a secret.** A token, a key, a password or a client secret goes in and is never read back; an
account's record says only what kind it is and which login it has.

| Route | What |
| --- | --- |
| `GET /api/v1/health` | `{ok, version, rclone, uptime_s}` |
| `GET /api/v1/state` | Everything the page shows: `{version, rclone, kinds: [{id, label, oauth}], accounts: [Account], login?: {id, url, expires}, system: {reachable, provider, encryption, targets, error?}, provider, provider_problem?, now}`. `system` is read from the system's API with the addon's own token (`system:read`), at most every 5 s (`?refresh=1`: now): whether the system knows the addon as a provider of backup targets, whether backup encryption is set up, and which backup targets use which account. `provider` says whether the socket the system reaches the addon through is served |
| `POST /api/v1/accounts` | Connects an account that needs no login in a browser: a `Spec` of kind `s3` or `webdav`, or `drive` with `service_account`. The account is looked at once (a listing); one that does not answer is not kept. `201` with the `Account` |
| `PUT /api/v1/accounts/{id}` | `{name, upload_limit_kbps}` (KiB/s, 0 = none) |
| `PUT /api/v1/accounts/{id}/credentials` | New keys for an account without a browser login: a `Spec` (its kind and name are the account's). The id stays, and with it every backup target that names the account; keys that do not work leave the old ones in place |
| `DELETE /api/v1/accounts/{id}` | Removes the account and its login on this system; nothing in the account's storage is touched. `409 in-use` with `targets: [{id, name}]` while a backup target of the system uses it; `?force=1` removes it all the same. `204` |
| `POST /api/v1/accounts/{id}/test` | Looks at the account (a listing, and its room where it says) and answers the `Account` with its new `state` |
| `POST /api/v1/login` | Starts a login at OneDrive, Google Drive or Dropbox: a `Spec` (`client_id` and `client_secret` when the user has a client of their own; Google Drive needs one), and `account` to log an existing account in again. `201 {id, url, expires, command}`: `url` is the provider's login address for the user's browser, `command` the line for a computer that has rclone. One login at a time (`409 login-busy`); it waits ten minutes |
| `POST /api/v1/login/finish` | Ends a login. In the browser: `{id, address}` - the address the provider sent the browser to after the login, `http://localhost:53682/?state=…&code=…`, which does not load there and is copied from the address bar. With rclone on another computer: `{token, spec, account?}` - what `rclone authorize` printed. `200` with the `Account`; `409 login-gone` when the login is not waiting any more |
| `DELETE /api/v1/login` | Ends the login that waits. `204` |
| `POST /api/v1/servers` | Adds an SSH server for copy jobs with rsync: `{name, host, port?, user, path}` (`path` is the base path on the server; a job writes below it). The addon makes an ed25519 key pair for it at once. `201` with the `Server`, whose `public_key` is the line for the server's `authorized_keys`. There is no password login |
| `PUT /api/v1/servers/{id}` | Changes it. Another host or port is another machine: its trusted host keys go |
| `DELETE /api/v1/servers/{id}` | Removes the server with its key; nothing on the server is touched. `409 in-use` with `jobs` while a copy job copies to it |
| `POST /api/v1/servers/{id}/key` | A new key pair: the old key stops working |
| `POST /api/v1/servers/{id}/hostkey` | Reads the server's host keys (`ssh-keyscan`) into `pending: [{type, fingerprint}]`. Nothing is trusted by that |
| `PUT /api/v1/servers/{id}/hostkey` | `{fingerprints: […]}` - the fingerprints the page showed and the user compared: when they are exactly the pending ones, those become the trusted `host_keys`. A run never accepts a host key by itself: an unknown or changed one fails it |
| `POST /api/v1/servers/{id}/test` | Tries the server as a job uses it: rsync over ssh writes a small file below the base path and removes it. The `Server` with its new `state` |
| `GET /api/v1/sources` | The folder picker: `?path=` a folder below the system's own storage (`/usr/local`; empty = its top). `{path, parent, self: Folder, folders: [Folder]}` with `Folder` `{name, path, state, why?, addon?, open}`: `state` is `ok` (selectable), `addon` (another addon's data: selectable, and the job then reads it with that addon's group, `addon`), `unreadable`, or `never` with `why`: `config` (all of `/usr/local/etc`), `temp`, `backups` (a folder that holds backups of the system), `program` (an addon's program files: a folder named `bin`, `lib`, `app`, `www` … inside an addon's tree that is tagged `.nobackup`), `own`, `root` |
| `POST /api/v1/jobs` | Makes a copy job: a `JobSpec`. Its `id` is made once from the name and never changes; its timer `local-sync-<id>` is made on the system with the schedule (switched off for `manual`). `201` with the `Job`. `409 unsupported` on a system that offers no timers to addons |
| `PUT /api/v1/jobs/{id}` | Changes it. The timer's text is written only when the schedule itself was chosen anew; `schedule.kind: "custom"` leaves a timer that was edited on the Services page alone. Choosing `hourly`, `daily` or `weekly` over such a timer answers `409 custom-timer` with its `calendar` unless `replace_custom: true` says the user confirmed it; `manual` only switches the timer off |
| `DELETE /api/v1/jobs/{id}` | Deletes the job, its timer and its history; nothing on the target is touched. `204` |
| `POST /api/v1/jobs/{id}/run` | `{dry_run?}`: starts the job's unit on the system, now (`202`). A dry run copies nothing and lists what a run would do. `409 dry-run-first` for a mirror job nobody has tried since it changed, `409 running` |
| `POST /api/v1/jobs/{id}/stop` | Stops the running job |
| `GET /api/v1/runs` | The history, newest first: `{runs: [Run]}`; `?job=` one job's |
| `GET /api/v1/runs/{job}/{run}` | `{run, released}`. With `?seen=1` the page says its user looked at the run: a mirror job's successful dry run that was looked at lets the job run for real and switches its timer on when it has a schedule (`released: true`) |
| `GET /api/v1/runs/{job}/{run}/log` | The run's file list: `{lines: [{kind, text}], more}`; `kind` is `C` copied, `R` replaced, `D` deleted, `E` an error's text. At most 5000 lines |

**`Account`:** `{id, name, kind, detail, root?, auth?, own_client?, created, state, text?, checked?, upload_limit_kbps,
last_upload?: {at, ok, name?, size?}, free_bytes?, total_bytes?, used_by: [{id, name}]}`. `id` is stable and opaque
(the system's backup target names the account by it); `detail` is a line such as "OneDrive · max@example.org";
`state` is one of `writable`, `unreachable`, `auth-failed`, `full`, `read-only`, `error` as the last test or transfer
left it, `text` rclone's line for a failure; `used_by` the system's backup targets on this account.

The state (`GET /api/v1/state`) also carries `servers: [Server]` and `copy: {supported, problem?, readable, jobs: [Job],
name_filters, fixed, root}`: whether the system offers timers to the addon, the addons whose data a job may read, the
jobs, and the two lists of what is left out (the default name filters a job can switch off, and what is never copied).
An `Account` carries `jobs`, the names of the copy jobs on it; such an account is not removed (`409 in-use`).

**`Server`:** `{id, name, host, port, user, path, created, public_key, host_keys: [{type, fingerprint}], pending?,
state, text?, checked?, jobs}`. `state`: `not-tested`, `ok`, `unreachable`, `auth-failed`, `hostkey-unknown`,
`hostkey-changed`, `no-rsync`, `read-only`, `error`. The private key is never in an answer.

**`JobSpec`:** `{name, source, read_addon?, target: {kind: "account"|"server", id, folder}, mode: "copy"|"mirror",
max_delete?, excludes?, name_filters?, bwlimit_kbps?, schedule: {kind: "manual"|"hourly"|"daily"|"weekly"|"custom",
time?, weekday?}}`. `source` is a folder below `/usr/local` that is not on the *never copied* list; `read_addon` the
addon whose group the run needs to read it (from the picker's `addon`). `copy` adds and replaces and deletes nothing;
`mirror` also deletes on the target what is gone from the source, at most `max_delete` files per run (50; 0 = no
limit). `excludes` are patterns with `*`, `?` and `**` (`*.tmp`, `/cache/**`); `name_filters` (on by default) leaves
out files that look like keys and tokens. There is no field for free options of rsync or rclone.

**`Job`:** the spec with `id`, `created`, `changed`, `needs_dry_run?`, and read from the system: `schedule` (as the
timer is now; `calendar` for a custom one), `timer: {unit, exists, enabled, next?, last?}`, `target_name`,
`target_gone?`, `last?: Run`, `running?: {run, started, dry_run, waiting?, files, bytes, percent, speed?, eta?}`.

**`Run`:** `{id, job, job_name, start, end, trigger: "timer"|"page", dry_run, tool: "rsync"|"rclone", result, copied,
replaced, deleted, errors, bytes, exit, error?, log_cut?}`. `result`: `ok`, `warnings`, `failed`, `stopped`,
`too-many-deletions`, `target-unreachable`, `login-expired`, `host-key-changed`, `quota-full`, `source-missing`,
`target-missing`, `busy`, `dry-run-first`.

**`Spec`:** `{name, kind}` and, by kind: `onedrive`, `dropbox`: `client_id?`, `client_secret?` (both or neither);
`drive`: `client_id`, `client_secret` - or `service_account` (the JSON key) and `folder_id` (the folder shared with
it); `s3`: `provider?` (`AWS`, `Minio`, `Wasabi`, `Cloudflare`, `DigitalOcean`, `Scaleway`, `Hetzner`, `IONOS`,
`Other`), `endpoint?` (required unless AWS), `region?`, `bucket`, `access_key`, `secret_key`; `webdav`: `url`,
`vendor?` (`nextcloud`, `owncloud`, `other`), `user`, `password`.

**Errors:** `{error, message}`: `422 invalid` (a field to correct), `404 not-found`, `409 in-use`, `login-busy`,
`login-gone`, `403` without the request header, and `502 account` with `state` when the account itself refused -
`auth-failed`, `unreachable`, `full`, `read-only` or `error`, with rclone's line as the message.

## The backup provider's socket

The six calls the system makes to hand over, list, read and delete backups are not part of this API: they are served
on the Unix socket `/run/occulite/backup-targets/sync/target.sock`, for the system's service alone, as openccu-lite's
provider contract defines them (occulited's `docs/backup-provider.md`).
