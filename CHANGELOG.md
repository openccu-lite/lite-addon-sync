# Changelog

## [0.2.0] - 2026-11-17

Copy jobs. For openccu-lite 2.0.0-beta.7 and newer (the first with timers an addon keeps itself).

- **Copy jobs** (the new tab): a folder of the system, copied in one direction to an account (with rclone) or to an
  SSH server (with rsync). *Copy* adds and replaces and deletes nothing on the target; *Mirror* also deletes there
  what is gone from the source - at most 50 files per run unless you say otherwise, and only after you have tried
  the job and looked at what it would do. *Try it* copies nothing and lists what a run would copy and delete.
  Patterns to leave out, a rate limit, *Run now*, *Stop*.
- **A simple schedule:** manual only, hourly, daily or weekly. Each job is a timer of the system
  (`local-sync-<job>`), so the Services page shows it, runs it, and edits its schedule in full; a timer edited
  there shows here as *custom* and is left alone until you choose to replace it. *Manual only* switches the timer
  off and keeps it.
- **What is copied, and what never is.** The folder picker shows the system's own storage. Never offered and never
  copied: the settings of the system and of every addon (`/usr/local/etc`, with their keys and passwords), backups
  of the system, temporary files, the addons' program files, this addon's own files. Files that look like keys and tokens are
  left out by default. Another addon's data can be chosen, and the job then reads it with that addon's rights - the
  dialog says what that opens.
- **SSH servers** for rsync, beside the accounts: the addon makes a key pair per server and shows the line for the
  server's `authorized_keys`; there is no password login. The server's host key is read, shown and trusted only
  after you confirmed it; a run never accepts a host key on the way. rsync and ssh are the system's own.
- **History:** every run with what it copied, replaced and deleted, its errors, and its file list; the runs are also
  in the system log under the job's unit.
- **No more privilege:** a job runs as the addon's user in a unit the system writes (the scope `timers:addon`, not
  `system:write`); the addon hands over the schedule and nothing else.

Not in it yet: keeping replaced and deleted files as versions, encryption of the copies on the target (rclone crypt),
a notice for a job that is overdue.

## [0.1.0] - 2026-11-17

The first version, for openccu-lite 2.0.0-beta.7 and newer (the first with backup targets an addon provides).

- **Cloud storage as a backup target.** The addon's accounts appear under *Add target* on the system's Backup page,
  beside the USB stick, the network share and the SSH server: the same card, the same schedule and retention, *Test*,
  *Back up now*, *Backups here* and *Restore this*. The system hands each backup to the addon over a Unix socket of
  its own (openccu-lite's provider contract, protocol 1); the addon uploads it under a temporary name, checks it and
  renames it. Backups reach an account **encrypted only** - the system sends nothing else to an addon.
- **Accounts:** OneDrive, Google Drive and Dropbox by a login at the provider - in the browser (open the login, copy
  the address of the page that cannot be loaded, paste it) or with `rclone authorize` on another computer; Google
  Drive with a client ID of your own or a service account; S3-compatible storage and WebDAV (Nextcloud) by keys.
  Test, log in again or change the keys (the account and every target on it stay), an upload limit, remove - an
  account a backup target uses is not removed unseen.
- **A login about to lapse** is said on the system's Status page two weeks before (OneDrive ends a login that was
  not used for 90 days), with the way to *Log in again*.
- **rclone 1.76.0** is in the package (upstream's release binary, pinned by its SHA-256), never in the system image.
  A workflow follows rclone's releases.
- **No privilege:** the addon runs as its own user, opens no port, reads nothing of the system and contacts nothing
  until an account is connected. Its programs are root's (`runtime.bin`), its logins are in its configuration
  directory (`0600`) and so in every backup of the system.
- German and English, light and dark.

