<p align="center"><img src="assets/logo.png" alt="Azor" width="220"></p>

# Azor

A small desktop telemetry recorder for Arch Linux and CachyOS. It remembers how your computer was doing – load, temperatures, disks, services, configuration and package changes – so you can ask later what happened. Hardware is detected automatically. No root, no network.

## Installation

Needs Go 1.22 or newer.

```sh
cd packaging/local
makepkg -si
systemctl --user enable --now azor
```

Or without root, from the project directory:

```sh
make install
```

Then run `azor help`.
