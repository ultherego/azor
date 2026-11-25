<div align="center">

<img src="assets/logo.png" alt="Azor logo" width="260">

<h1>Azor</h1>

<p><b>A Claude Code hook that knows what your computer has been doing</b></p>

<p>
<a href="https://archlinux.org"><img src="assets/archlinux.svg" alt="Arch Linux" title="Arch Linux" height="40"></a>
&nbsp;&nbsp;&nbsp;
<a href="https://cachyos.org"><img src="assets/cachyos.svg" alt="CachyOS" title="CachyOS" height="40"></a>
</p>

</div>

Azor is a hook for Claude Code, backed by a small telemetry daemon. The daemon records load, temperatures, disks, services, configuration and package changes, also while Claude is closed. The hook gives Claude a one-line summary when a session starts, and a skill lets it look up the history when you ask what happened. Hardware is detected automatically. No root; Azor itself makes no network connections, but what the hook prints becomes part of your Claude conversation.

## Installation

Needs Go 1.22 or newer.

```sh
git clone https://github.com/ultherego/azor.git
cd azor/packaging/local
makepkg -si
systemctl --user enable --now azor
```

Or without root, from the project directory:

```sh
make install
```

Then connect it to Claude Code as described in [claude/README.md](claude/README.md).
