# granola-export

A command-line tool for macOS that copies your notes from the [Granola](https://www.granola.ai) app into a folder as Markdown files that Obsidian reads as notes with properties, and keeps that folder up to date once a day.

It is a one-way mirror: Granola is the source, and files in your folder are created and updated but never deleted. A file you edited after it was exported is not overwritten.

> **Status: not yet run against a real Granola install.** It was developed and tested on Linux against made-up data shaped after public descriptions of Granola's files and private API. Run `granola-export doctor` and then `granola-export sync --out <folder> --dry-run` first; both only read. See [Limits](#limits).

## Install

You need [Go](https://go.dev/dl/) 1.22 or later to build it.

```bash
git clone https://github.com/YitianChenJKZ/granola-export.git
cd granola-export
make build
mkdir -p ~/bin && cp bin/granola-export ~/bin/
```

Put the binary somewhere it can stay (`~/bin`, `/usr/local/bin`), because the daily job points at its path. `make dist` builds binaries for Apple Silicon and Intel Macs into `dist/`.

## Use

Check what the tool can read on this Mac. This prints counts, versions and times, never your login token or note content:

```bash
granola-export doctor
```

See what a sync would write, without writing anything (this does contact Granola to read your notes):

```bash
granola-export sync --out ~/Documents/MyVault/Granola --dry-run
```

Export:

```bash
granola-export sync --out ~/Documents/MyVault/Granola
```

Sync every day at 09:00 (pick another time with `--at HH:MM`):

```bash
granola-export schedule install --out ~/Documents/MyVault/Granola
granola-export schedule status
granola-export schedule uninstall
```

### `sync` flags

| Flag | Meaning |
| --- | --- |
| `--out <folder>` | Folder to export into. Defaults to the folder saved by `schedule install`. |
| `--source auto\|app\|cache` | Where to read notes from (see below). Default `auto`. |
| `--no-transcript` | Leave transcripts out. |
| `--since YYYY-MM-DD` | Only notes created on or after this date. |
| `--dry-run` | Show what would be written, write nothing. |
| `--overwrite-edited` | Replace exported files you edited when their note changed in Granola. |
| `--granola-dir <folder>` | Granola's data folder, if it is not `~/Library/Application Support/Granola`. |

`sync` exits with 0 on success, 1 if anything failed, and 2 for a mistake on the command line.

## What a note looks like

```markdown
---
title: "Quarterly budget review"
date: 2026-11-17
created: 2026-11-17T15:30:00
updated: 2026-11-17T16:45:00
granola_id: f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c
granola_url: https://notes.granola.ai/d/f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c
attendees:
  - "Oat Benson"
  - "Raisin Patel"
tags:
  - granola
---

## Summary

### Decisions

- Cut marketing spend by 10%

## My notes

- push back on the dairy forecast

> [!quote]- Transcript
> **Me** 15:30: Shall we start with the forecast?
>
> **Them** 15:30: Sure. Numbers are mostly in.
```

- **Summary** is Granola's AI-generated notes, **My notes** is what you typed. A note with several AI panels gets one section per panel, titled as in Granola.
- The transcript is a callout that Obsidian shows collapsed. What your microphone picked up is labelled **Me**, the rest of the meeting audio **Them**.
- Dates and times are in your Mac's time zone.
- The file is named `YYYY-MM-DD Title.md`, without the characters Obsidian and macOS do not allow in names or links. Two notes that would get the same name are told apart by a few characters of their Granola id.
- Notes with no AI notes, no typed notes and no transcript (such as untouched calendar placeholders) are not exported.

## How syncing behaves

- A file is tied to its Granola note by the `granola_id` property. You can rename the file or move it to another folder inside the output folder; later syncs find it and update it where it is. Keep that property.
- The file name is chosen once. If the title changes in Granola, the `title` property changes and the file name stays, so links to the note keep working.
- **Changed in Granola, file untouched:** the file is replaced with the new version.
- **Changed in Granola, file edited by you:** the file is left alone and listed in the output on every sync until you pass `--overwrite-edited`. A file counts as edited if it differs at all from what was written, including changes Obsidian makes when you edit properties.
- **Edited by you, unchanged in Granola:** left alone.
- **Deleted in Granola:** the exported file stays.
- **Deleted from the folder:** the file is created again on the next sync.
- The record of what was written is kept in `<folder>/.granola-export/state.json`. Obsidian does not show folders whose name starts with a dot. If it is lost, files identical to a fresh export are picked up again and all others are treated as edited.
- Granola's own files are only ever read.

## Where notes are read from

With `--source auto`, the default, the tool tries these in order:

1. **Granola's login** (`app`). The Granola app keeps a login token in `~/Library/Application Support/Granola/supabase.json`. The tool reads it and asks Granola's servers for your notes, the AI notes and the transcript, the way the app does. The token is sent only to `api.granola.ai`. It is not stored anywhere else and not refreshed: refreshing would replace the app's own session.
2. **Granola's local cache** (`cache`), the file `cache-v*.json` in the same folder, when the login is missing or expired or Granola cannot be reached. Recent versions of Granola do not keep the AI notes in this file, so notes exported from the cache usually hold only your typed notes and the transcript. The next sync that can use the login completes them. A cache sync never replaces a file that a login sync wrote.

To spare Granola's servers, the AI notes and transcript are requested only for notes that are new, changed, missing from the folder, or less than seven days old. Requests go out one at a time and back off when Granola asks for it. A first sync of a long history can take several minutes. If you later add or drop `--no-transcript`, older files that are not fetched again keep their previous form.

## Daily schedule

`schedule install` saves the folder and options to `~/Library/Application Support/granola-export/config.json`, writes a LaunchAgent to `~/Library/LaunchAgents/com.granola-export.sync.plist` and loads it with `launchctl`.

- It runs at the chosen time every day and each time you log in. If the Mac is asleep at that time, launchd runs it when the Mac wakes. The first run starts as soon as you install.
- Output is appended to `~/Library/Logs/granola-export.log`.
- The login token is short-lived and Granola renews it while it runs. If Granola has not been open recently, that day's sync falls back to the cache and says so in the log.
- `schedule uninstall` removes the job. Your notes, the settings file and the log stay.

### macOS permissions

The scheduled job runs in the background. If your vault is in `~/Documents`, `~/Desktop`, iCloud Drive or another protected location, macOS may ask once whether `granola-export` may access it, or may block it without asking. If the log shows "operation not permitted", add the binary under System Settings → Privacy & Security → Full Disk Access (or Files and Folders). Running `granola-export sync` once by hand in Terminal first is a good check that the export itself works.

If macOS refuses to open a binary you downloaded rather than built, it has been quarantined; building it yourself with `make build` avoids that.

## Limits

- **Untested on a real Mac.** The layout of `supabase.json`, of the cache (versions 3 and 4 and later) and of the API responses was taken from public write-ups by other people, not from a Granola install. `launchctl` has not been run by this code on macOS either. `doctor` and sync errors name the keys and types they found when the layout differs, without printing values, to make such differences quick to fix.
- **Private API.** The endpoints used with Granola's login are undocumented and can change or stop working without notice. Whether they insist on headers this tool does not send is not known. The tool identifies itself as `granola-export` and sends the installed Granola version if it finds the app.
- **"Not found" answers.** If Granola answers "not found" for the AI notes or transcript of a note, the note is exported without that part and the output says how many notes were affected. A file that already contains the part is left as it is rather than stripped.
- **Encrypted storage.** Granola can keep its login and cache encrypted behind the Keychain (`*.json.enc`). The tool detects that and stops with a message; it does not decrypt them.
- **Stale cache.** Granola rewrites its cache file only now and then (reportedly at startup), so the cache fallback can be behind.
- **Not exported:** notes other people shared with you, attachments and screenshots, folders.
- Granola has an official API for Business and Enterprise workspaces. This tool does not use it.

## Development

```bash
make test    # go vet and go test
make build   # bin/granola-export for this machine
make dist    # dist/ binaries for macOS arm64 and amd64
```

The code has no dependencies outside the Go standard library.

- `cmd/granola-export`: the commands.
- `internal/granola`: reading the login token, the API client, the cache reader.
- `internal/prosemirror`: Granola's rich text (ProseMirror JSON) to Markdown.
- `internal/obsidian`: frontmatter, file names, note layout.
- `internal/sync`: deciding what to create, update or leave alone.
- `internal/schedule`: the launchd job.
