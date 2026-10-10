# Oopsy

**Undo for `rm -rf`, and everything else you wish you hadn't run.** Files (deletes and overwrites), git (resets, branches, stashes, force pushes) and Postgres (drops, deletes, bad updates). Oopsy snapshots it right before it is destroyed, whether you typed the command or your AI agent did. Type `oops` and it is back.

Website: https://sohan-bhat.github.io/oopsy/

Runs on macOS, Linux and Windows.

```console
$ rm -rf src
oops: saved snapshot 12 (214 items, 3.1 MB). Run oops to undo.

$ oops
Snapshot 12  just now  from zsh
  $ rm -rf src
  in ~/code/app

This will bring back:
  ~/code/app/src  folder, 213 items, 3.1 MB

Restore it? [y/N] y
  restored ~/code/app/src
Restored snapshot 12.
```

Ctrl+Z does not cover the terminal, `rm` skips the Trash, git only protects what you committed, and agent checkpoints do not track files changed by shell commands or rows in your database. Oopsy covers all of those.

## Install

macOS and Linux:

```sh
brew install sohan-bhat/tap/oopsy
```

or

```sh
curl -fsSL https://sohan-bhat.github.io/oopsy/install.sh | sh
```

Windows:

```powershell
scoop bucket add oopsy https://github.com/sohan-bhat/scoop-bucket
scoop install oopsy/oopsy
```

or

```powershell
irm https://sohan-bhat.github.io/oopsy/install.ps1 | iex
```

To pin a version, set `OOPS_VERSION` (without the `v`):

```sh
curl -fsSL https://sohan-bhat.github.io/oopsy/install.sh | OOPS_VERSION=0.2.0 sh
```

```powershell
$env:OOPS_VERSION = '0.2.0'; irm https://sohan-bhat.github.io/oopsy/install.ps1 | iex
scoop install oopsy/oopsy@0.2.0
```

Homebrew always installs the latest release.

Then turn on the hooks once:

```sh
oops init
```

This adds a small hook to zsh, bash, fish and PowerShell (Git Bash too on Windows), and a pre tool hook to Claude Code, Codex CLI, Cursor and Gemini CLI if they are installed. Every file it touches is backed up first, and `oops uninstall` removes it all.

## Use

```sh
oops                 # undo the last destructive command (asks first, -y to skip)
oops list            # recent snapshots
oops undo 12         # restore a specific one; oops show 12 and oops diff 12 to look first
oops doctor          # check hooks, copy on write support and hook speed
```

For Postgres, run your app or agent through the proxy:

```sh
oops db wrap -- npm run dev          # or: oops db proxy --upstream postgres://...
oops db list
oops db undo
```

## How it works

The shell hook runs right before each command. A quick regex skips anything harmless without starting a process. Otherwise Oopsy parses the command with a real shell parser, expands variables and globs the way your shell will, and works out exactly which paths, git refs or stashes are about to be destroyed. It saves them under `~/.oops` the cheapest safe way the disk allows:

* a copy on write clone: APFS on macOS, reflinks on btrfs and xfs, block cloning on ReFS and Dev Drive. Instant, and no extra disk until the original changes.
* hard links when the command only deletes (`rm`, `Remove-Item`, `del`, `git clean`), on NTFS and ext4. Also instant and free, since the data simply stays alive through the snapshot.
* a real copy for overwrites where neither works: NTFS compressed on Windows, zstd compressed elsewhere (files that are already compressed, like images and archives, are stored as they are). A copy can be any size as long as the disk keeps a safe floor free afterwards: 5% of the disk, at least 2 GB and at most 20 GB, which is about where ext4 keeps space back for root and where macOS and Windows start warning. If a copy would break the floor, Oopsy skips it and tells you right then. Change the floor with `oops config free_floor_gb 4`.

When the folder is on another drive than your home, hard links and clones go to a private store at that drive's root (`D:\.oops` on Windows, readable only by you). Snapshots are pruned after 7 days or past 5 GB (`oops config` to change).

PowerShell has no preexec hook, so Oopsy wraps the function PSReadLine uses to read each line and checks it after you press Enter, before it runs. The PowerShell parser knows `Remove-Item` and its aliases, `Move-Item`, `Copy-Item`, `Set-Content`, `Out-File`, redirects and pipelines like `Get-ChildItem *.log | Remove-Item`. Agent commands on Windows are read as bash, PowerShell and cmd.exe at once.

Restores never overwrite newer work: if something exists at the path again, both copies are kept.

The Postgres proxy speaks the wire protocol and parses every statement. Before a `DELETE`, `UPDATE`, `TRUNCATE`, `DROP TABLE`, `DROP SCHEMA` or `ALTER TABLE ... DROP COLUMN` it copies exactly the affected rows, tables or column into an `oops` schema inside the same transaction, so a rollback discards the snapshot too.

## What it does not catch

* Programs that delete things themselves. Oopsy reads the command line, so `make clean`, `npm run reset` or a Python script that removes files are invisible to it.
* Anything remote: `ssh`, `rsync` to another host, cloud resources, `kubectl delete`, `docker volume rm`, `terraform destroy`.
* File edits made by an editor or by an agent's own edit tool rather than the shell.
* Database changes that do not go through the proxy, plus `DROP DATABASE`, `DROP INDEX`, `DROP VIEW`, deletes inside functions or triggers, and upserts. Grants and row level security policies are not restored. An `UPDATE` on a table without a primary key is undone by restoring the whole table.
* Overwrites on a disk without copy on write (NTFS, ext4) when the copy would leave less than the free space floor: Oopsy skips that copy and tells you.
* Undoing `git push --force` needs the old commit in your local repo, which it is if you fetched before pushing.

On Windows:

* Commands typed into cmd.exe are not hooked (cmd has no hook to run code before a command). Commands agents run through cmd are covered.
* Files another program holds open without sharing are skipped with a warning.
* Copies and clones do not keep alternate data streams (such as the downloaded from the internet mark); hard link snapshots do. ACLs come from the folder a file is restored into.
* Windows PowerShell's default execution policy on desktop Windows blocks profiles; `oops init` and `oops doctor` tell you the one command to allow it.

The Linux reflink and Windows paths are newer and have had less real world use than macOS.

## Exit codes

`oops` and `oops db` exit with 0 when they are done (or there was nothing to do), 1 when something went wrong, 2 when the command line was wrong, 3 when the snapshot or database change you asked for does not exist, and 4 when a restore brought back some paths but not all. Every error says what happened and what to do next.

## License

MIT
