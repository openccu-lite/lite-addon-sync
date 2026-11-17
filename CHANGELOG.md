# Changelog

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
- **rclone 1.76.0** is in the package (upstream's release binary, pinned by its SHA-256), never in the system image.
  A workflow follows rclone's releases.
- **No privilege:** the addon runs as its own user, opens no port, reads nothing of the system and contacts nothing
  until an account is connected. Its programs are root's (`runtime.bin`), its logins are in its configuration
  directory (`0600`) and so in every backup of the system.
- German and English, light and dark.

Not in it yet: copy jobs for folders of the system (with rsync and a schedule) - the next step.
