# Oopsy

**Undo for `rm -rf`, and everything else you wish you hadn't run.** Files (deletes and overwrites, by any command, including `make clean`, npm scripts and scripts Oopsy has never seen), git (resets, branches, stashes, force pushes) and Postgres (drops, deletes, bad updates). Oopsy snapshots it right before it is destroyed, whether you typed the command or your AI agent did. Type `oops` and it is back.

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

Oopsy has two layers.

**The command line layer** is fast and precise. The shell hook runs right before each command. Oopsy parses it with a real shell parser, expands variables and globs the way your shell will, and works out exactly which paths, git refs or stashes are about to be destroyed, including paths outside your project.

**The project mirror** catches everything else, so a program that deletes or rewrites files by itself (`make clean`, `npm run reset`, a Python script) is undoable too. For every project you work in (the git work tree around you, or the folder itself), Oopsy keeps a standing mirror that costs almost no disk: each file is a copy on write clone or a hard link of the original, plus a compact index. Before a command that is not provably read only, a quick stat walk brings the mirror up to date (about 20 to 30 ms for 10,000 files, see the numbers below). After the command, the walk runs again in the background at your next prompt, and every file the command changed or removed moves from the mirror into that command's snapshot. `oops` undoes it exactly, swapping changed files back while they are still what the command left.

* Read only lines (`ls`, `cat`, `grep`, `git status`, `git log` and friends) are skipped inside the shell itself, at no cost.
* Rebuildable folders (`node_modules`, `target`, `.venv`, `venv`, `dist`, `build`, `.next`, `__pycache__`, `.gradle`, and anything in a `.oopsignore`) are not mirrored file by file. When a command is about to touch one, because it names it or a script it runs does (Oopsy reads npm, yarn, pnpm and bun scripts in `package.json`, asks `make -n` for its plan, and reads script files the command runs), the whole folder is saved right then as one clone or hard link tree, and kept for a day instead of seven.
* The first time you run a command in a big project (over 3,000 files), the mirror is built in the background; until then only the command line layer covers it. Projects over 50,000 files are left to the command line layer (`oops config mirror_max_files` changes that).

Snapshots are saved the cheapest safe way the disk allows:

* a copy on write clone: APFS on macOS, reflinks on btrfs and xfs, block cloning on ReFS and Dev Drive. Instant, and no extra disk until the original changes; an overwrite then keeps only the changed blocks.
* hard links on NTFS and ext4. Also instant and free, since the data stays alive through the snapshot. A hard link shares the file, so a program that writes into a file in place (instead of replacing it) would change the link too. On Linux, Oopsy's shells load a small preload library that copies a mirrored file right before a program first writes into it. Statically linked programs, like most Go binaries, do not load preload libraries.
* a real copy for overwrites the command line names, where neither works: NTFS compressed on Windows, zstd compressed elsewhere (files that are already compressed are stored as they are). A copy can be any size as long as the disk keeps a safe floor free afterwards: 5% of the disk, at least 2 GB and at most 20 GB, which is about where ext4 keeps space back for root and where macOS and Windows start warning. If a copy would break the floor, Oopsy skips it and tells you right then. Change the floor with `oops config free_floor_gb 4`.

When the folder is on another drive than your home, hard links and clones go to a private store at that drive's root (`D:\.oops` on Windows, readable only by you). Snapshots are pruned after 7 days or past 5 GB (`oops config` to change).

PowerShell has no preexec hook, so Oopsy wraps the function PSReadLine uses to read each line and checks it after you press Enter, before it runs, and runs the after step from the prompt function. The PowerShell parser knows `Remove-Item` and its aliases, `Move-Item`, `Copy-Item`, `Set-Content`, `Out-File`, redirects and pipelines like `Get-ChildItem *.log | Remove-Item`. Agent commands on Windows are read as bash, PowerShell and cmd.exe at once.

Infrastructure commands get their state saved first, by the tool itself, with a timeout so they are never held up:

* `kubectl delete`, `apply` and `replace` (and `oc`): the exact targets are saved as JSON with `kubectl get`, pinned to the current context. Undo recreates deleted objects through the API and puts changed ones back; objects an apply created are listed with the command to delete them, never deleted for you. `kubectl delete namespace` also saves the common objects inside it. Data inside volumes is not saved, so a recreated volume claim comes back empty, and undo says so.
* `terraform` and `tofu` `destroy`, `apply` and `state rm` or `mv`: the state is saved with `state pull` and pushed back with a newer serial, never with `-force`. Only the state comes back: resources that were destroyed must be recreated with `terraform apply`, and data inside them (database rows, bucket objects, disks) was not saved.
* `docker volume rm` and `prune` (and `podman`): the volume's files are saved as a zstd compressed tar under the free space floor, and undo recreates the volume with its driver, options and labels.
* `helm uninstall`: the release's records and live resources are saved, and undo brings the release back into `helm list` at the same revision. Resources made by chart hooks are not saved.

Restores never overwrite newer work: if something exists at the path again, both copies are kept.

The Postgres proxy speaks the wire protocol and parses every statement. Before a `DELETE`, `UPDATE`, `TRUNCATE`, `DROP TABLE`, `DROP SCHEMA` or `ALTER TABLE ... DROP COLUMN` it copies exactly the affected rows, tables or column into an `oops` schema inside the same transaction, so a rollback discards the snapshot too.

## What it does not catch

* A program that writes into a file in place on NTFS, or a statically linked program doing so on ext4. The mirror there uses hard links, which share the file, so Oopsy notices the change but cannot keep the old version, and says so.
* A rebuildable folder like `node_modules` deleted by a program that does not name it anywhere Oopsy can read. Rebuild it.
* Commands run right in your home folder or a system folder, and projects over 50,000 files: only what the command line names is saved there.
* Other machines: commands run over `ssh` or `rsync` to another host change another computer, so undo there needs Oopsy installed there too.
* Data inside cloud resources and Kubernetes volumes. Oopsy saves Kubernetes objects, Terraform state, Docker volumes and Helm releases, not the rows, objects or disks inside cloud resources.
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
