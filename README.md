<div align="center">

<img src="assets/logo.png" alt="Azor logo" width="260">

<h1>Azor</h1>

<p><b>A small desktop telemetry recorder for Arch Linux and CachyOS</b></p>

</div>

Azor remembers how your computer was doing – load, temperatures, disks, services, configuration and package changes – so you can ask later what happened. Hardware is detected automatically. No root, no network.

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

Then run `azor help`.
