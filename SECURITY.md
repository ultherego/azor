# Security

## Reporting

Please report a vulnerability privately through GitHub: **Security → Report a vulnerability** on this repository. Do not open a public issue for it.

## Verifying the code

Commits on `main` are signed with the maintainer's SSH key and shown as **Verified** on GitHub; force pushes and deleting `main` are blocked. Treat an unsigned commit on `main` as suspicious.

## What Azor protects

- **No root, no network.** The daemon runs as your user, in a systemd sandbox that allows only local (Unix) sockets and a read-only view of the system.
- **No secrets are read.** Places that commonly hold keys, passwords or tokens are blocked twice: by a hard list in the code (`forbidden.go`), which also applies to your own `watch.files`, and by `InaccessiblePaths` in the unit. Files are never opened: changes are seen from size, times and inode.
- **No contents.** History holds numbers, names of programs, units, devices and configuration files, and events. No command lines, window titles, clipboard, screen contents or log messages.
- **Untrusted names.** Names chosen by other programs are cleaned of control and invisible characters when stored and shown. The hook line for Claude keeps only plain characters, cuts names short and says that names are data.
- **History stays local** in `~/.local/state/azor`, readable only by you. What the hook or a command prints goes into a Claude conversation.

## Limits

- Azor recognizes sensitive places by name. A secret kept under a neutral name in a file you add to `watch.files` is not recognized, although the file is still never opened.
- Anyone who can write to your home directory can change Azor, its configuration or your Claude hooks. Keep them writable only by you.
- The hook output is untrusted data for Claude. The skill says so, but a model can still be misled; do not grant Claude broad permissions on the strength of what the hook says.
