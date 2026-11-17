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

**`Account`:** `{id, name, kind, detail, root?, auth?, own_client?, created, state, text?, checked?, upload_limit_kbps,
last_upload?: {at, ok, name?, size?}, free_bytes?, total_bytes?, used_by: [{id, name}]}`. `id` is stable and opaque
(the system's backup target names the account by it); `detail` is a line such as "OneDrive · max@example.org";
`state` is one of `writable`, `unreachable`, `auth-failed`, `full`, `read-only`, `error` as the last test or transfer
left it, `text` rclone's line for a failure; `used_by` the system's backup targets on this account.

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
