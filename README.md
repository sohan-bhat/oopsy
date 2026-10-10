# Oopsy

**Undo for the terminal.** Oopsy snapshots your files, git state and Postgres data right before a destructive command runs, whether you typed it or your AI agent did. Type `oops` and it is back.

Website: https://sohan-bhat.github.io/oopsy/

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

```sh
brew install sohan-bhat/tap/oopsy
```

or

```sh
curl -fsSL https://raw.githubusercontent.com/sohan-bhat/oopsy/main/install.sh | sh
```

Then turn on the hooks once:

```sh
oops init
```

This adds a small hook to zsh, bash and fish, and a pre tool hook to Claude Code, Codex CLI, Cursor and Gemini CLI if they are installed. Every file it touches is backed up first, and `oops uninstall` removes it all.

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

The shell hook runs right before each command. A quick regex skips anything harmless without starting a process. Otherwise Oopsy parses the command with a real shell parser, expands variables and globs the way your shell will, and works out exactly which paths, git refs or stashes are about to be destroyed. It saves them under `~/.oops` with an APFS clone on macOS or a reflink on btrfs and xfs, so a snapshot is instant and takes almost no disk until the original is gone. Snapshots are pruned after 7 days or past 5 GB (`oops config` to change).

Restores never overwrite newer work: if something exists at the path again, both copies are kept.

The Postgres proxy speaks the wire protocol and parses every statement. Before a `DELETE`, `UPDATE`, `TRUNCATE`, `DROP TABLE`, `DROP SCHEMA` or `ALTER TABLE ... DROP COLUMN` it copies exactly the affected rows, tables or column into an `oops` schema inside the same transaction, so a rollback discards the snapshot too.

## What it does not catch

* Programs that delete things themselves. Oopsy reads the command line, so `make clean`, `npm run reset` or a Python script that removes files are invisible to it.
* Anything remote: `ssh`, `rsync` to another host, cloud resources, `kubectl delete`, `docker volume rm`, `terraform destroy`.
* File edits made by an editor or by an agent's own edit tool rather than the shell.
* Database changes that do not go through the proxy, plus `DROP DATABASE`, `DROP INDEX`, `DROP VIEW`, deletes inside functions or triggers, and upserts. Grants and row level security policies are not restored. An `UPDATE` on a table without a primary key is undone by restoring the whole table.
* Very large trees on filesystems without copy on write (ext4, external drives): Oopsy falls back to a plain copy capped at 1 GB and tells you when it skipped something.
* Undoing `git push --force` needs the old commit in your local repo, which it is if you fetched before pushing.

The Linux reflink path is new and has had less testing than macOS.

## License

MIT
