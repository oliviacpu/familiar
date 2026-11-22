# familiar

A small program that reports a computer's load and health to [Home Assistant](https://www.home-assistant.io). It runs on macOS, Windows and Linux, as a menu bar or tray icon, or headless in a container.

It registers with Home Assistant the way the phone app does (the built-in `mobile_app` integration), so each computer becomes a device with its own sensors. No add-on or custom integration is needed.

## What Home Assistant gets

Each computer gets the sensors it can measure:

| Sensor | |
|---|---|
| `CPU`, `Memory`, `GPU` | % in use |
| `CPU temperature`, `GPU temperature` | °C |
| `Disk` | % used on the main drive |
| `Disk <name>` | % used on each other drive built into the machine: `Disk D:` on Windows, the volume's name on a Mac, the mount folder on Linux. Attributes: `path`, `size_gb`, `free_gb` |
| `Drive problem` | on when any drive reports trouble (see below) |
| `Network down`, `Network up` | Mbit/s |
| `Last boot`, `Battery`, `Charging` | |
| `Activity` | `Busy` or `Idle` |

**Drives** are found on every reading, so one that's added appears within a reading and one that's removed shows as unknown. Plugged-in drives (USB, SD cards, external Mac drives), network shares, CDs and DVDs, read-only volumes such as opened disk images, macOS system volumes and Linux `/boot` are left out. `disks` in the settings file (or `FAMILIAR_DISKS`, comma-separated) lists drives instead of finding them; listed paths must be mount points.

**Drive health** is each built-in drive's own report, read every 10 minutes the way smartctl and CrystalDiskInfo read it:

- **NVMe** (Windows and macOS): the SMART / health log. A problem is any critical warning (spare space low, too hot, reliability degraded, read-only), any media errors, or 90% or more of the drive's rated life used.
- **SATA** (Windows): SMART attributes and thresholds. A problem is a pre-fail attribute at its threshold, any reallocated, pending or uncorrectable sectors, or 90% wear. Wear is read only where the attribute is known to mean wear for that maker (Samsung, Intel, Crucial/Micron).
- **Other Mac drives:** macOS's SMART verdict.
- **Linux:** not yet; use smartctl on the host.

`Drive problem` has the attributes `status` (`Healthy`, or e.g. `Disk 2: 1 pending sector`) and `disks`: per drive the device (`Disk N` as in Disk Management, or `diskN`), model, bus, status, temperature, wear (% of rated life used) and power-on hours. Windows needs administrator rights for this; the scheduled task that `familiar install` creates has them.

**Activity** is `Busy` when the median CPU or GPU load over the last four readings (two minutes) is at or above a threshold: 25% CPU or 20% GPU by default. Memory isn't counted, because apps keep memory while idle. A `history_stats` helper in Home Assistant can turn it into hours busy per day.

Readings stop while a computer sleeps; Home Assistant keeps the last value, so a template helper that checks how recent the last report is (`last_reported`) is useful for an on/off status.

## Install

Download the build for the computer from the [latest release](https://github.com/oliviacpu/familiar/releases/latest):

- **macOS** (Apple silicon and Intel): `familiar-macos`. Put it somewhere it can stay, e.g. `mkdir -p ~/Applications && mv ~/Downloads/familiar-macos ~/Applications/familiar && chmod +x ~/Applications/familiar`. If a browser downloaded it, also run `xattr -d com.apple.quarantine ~/Applications/familiar`.
- **Windows:** `familiar-windows.exe`, e.g. as `%LocalAppData%\Programs\familiar\familiar.exe`.
- **Linux:** `familiar-linux-amd64` or `familiar-linux-arm64`, e.g. as `~/.local/bin/familiar`.

Connect it to Home Assistant with a long-lived access token (Profile → Security), which is used once to register and isn't saved:

```
familiar setup --url http://homeassistant.local:8123 --token <token> --name <computer name>
```

Then start it at sign-in, and now:

```
familiar install
```

- **macOS:** a LaunchAgent (`~/Library/LaunchAgents/zone.olivia.familiar.plist`), logging to `~/Library/Logs/familiar.log`, with an icon in the menu bar.
- **Windows:** one administrator prompt, then it installs [PawnIO](https://pawnio.eu) if it's missing (for the CPU temperature) and creates the scheduled task **Familiar**, which starts it at sign-in with administrator rights and no prompt. It logs to `%LocalAppData%\familiar\familiar.log`. Icon in the notification area.
- **Linux desktop:** a systemd user service (`~/.config/systemd/user/familiar.service`) that starts and stops with the desktop session and restarts familiar if it crashes, logging to the journal (`journalctl --user -u familiar`). The icon goes in the panel (StatusNotifierItem; GNOME needs the AppIndicator extension).

`familiar uninstall` removes the login item, task or service again. `familiar once` prints a reading without sending it, `familiar drives` prints each drive's health, and `familiar temps` lists the temperature sensors it found.

Settings are in `config.json` in the user's config folder (`~/Library/Application Support/familiar`, `%AppData%\familiar`, `~/.config/familiar`). Every setting can also come from a `FAMILIAR_*` variable; see `config.go`.

## The menu

The icon's menu is titled with the computer's name and when it last reported, with any sending problem under it. The readings that have somewhere to look closer open it when clicked: CPU and activity open Activity Monitor (on its CPU tab), Task Manager or the system monitor; memory, storage, GPU, network and battery open their own tab or settings page. The rest are labels.

## Updates

Installed copies update themselves. A few minutes after starting, and every 6 hours after that, familiar checks the latest release here. If there's a newer version it downloads the build for its platform and installs it only if:
1. the release's `SHA256SUMS` carries a valid signature from the release key built into familiar, and
2. the download matches its checksum in that file.

Then it restarts into the new version. Set `"auto_update": false` (or `FAMILIAR_AUTO_UPDATE=false`) to turn this off. The container image never updates itself; pull a new tag instead.

## Container

```yaml
services:
  familiar:
    image: ghcr.io/oliviacpu/familiar:latest
    restart: unless-stopped
    network_mode: host          # the host's own network traffic
    runtime: nvidia             # optional: NVIDIA GPU load and temperature
    read_only: true
    volumes:
      - ./familiar.json:/config/config.json:ro   # from `familiar setup`, holds the webhook
      - /srv/storage:/srv/storage:ro             # any other drives to report
    environment:
      FAMILIAR_CONFIG: /config/config.json
      FAMILIAR_URL: http://192.0.2.10:8123
      FAMILIAR_NAME: server
      FAMILIAR_DISKS: /srv/storage
      NVIDIA_VISIBLE_DEVICES: all
      NVIDIA_DRIVER_CAPABILITIES: utility
```

In a container `/` is the host's main drive (through the container's own filesystem). Other drives are only visible when mounted.

## How it talks to Home Assistant

After `setup`, a computer holds only Home Assistant's address and its own webhook ID, which can update that computer's sensors and nothing else. Use an address that works wherever the computer goes (for a laptop, one that's reachable away from home).

## CPU temperature on Windows

Windows gives ordinary programs no access to the CPU's temperature: it is in CPU registers only kernel code can read. familiar asks the signed [PawnIO](https://pawnio.eu) driver, which LibreHardwareMonitor and Fan Control also moved to after Microsoft blocked WinRing0. It uses PawnIO's official modules, which can only read a fixed list of registers:

- **Intel:** TjMax from `IA32_TEMPERATURE_TARGET` (`0x1A2`), minus the package readout in `IA32_PACKAGE_THERM_STATUS` (`0x1B1`).
- **AMD Zen:** SMN `0x59800`, read the way Linux's k10temp does.

Without PawnIO or administrator rights it reports everything else.

- `pawnio/IntelMSR.bin` and `pawnio/AMDFamily17.bin`: from [PawnIO.Modules 0.2.11](https://github.com/namazso/PawnIO.Modules/releases/tag/0.2.11), LGPL-2.1 (`pawnio/COPYING`).
- `PawnIO_setup.exe`: [PawnIO.Setup 2.2.0](https://github.com/namazso/PawnIO.Setup/releases/tag/2.2.0), signed by namazso. It isn't kept in git; the workflow downloads it and checks its SHA-256 (in `.github/workflows/release.yml`). For a local Windows build, put it in `pawnio/` the same way.

## Icon

The cat is [Fluent UI System Icons](https://github.com/microsoft/fluentui-system-icons)' `animal_cat_28_filled` (MIT). `icons/render.py` renders `icons/familiar.svg` to the macOS menu bar template, the Windows `.ico` and the Linux PNG.

## Building

`.github/workflows/release.yml` runs on every push. It tests, then builds a universal macOS binary (on a Mac runner, because the menu bar icon needs Apple's compiler), the Windows `.exe` (`-H=windowsgui`, so no console window), Linux binaries and the container image. On `main` it also publishes the release, with `SHA256SUMS` signed by `cmd/familiar-sign` using the `FAMILIAR_SIGNING_KEY` secret.

Locally, without installing Go: `docker run --rm -v "$PWD:/src" -w /src golang:1.27 go test ./...`

The parsers for each OS's tools and data (`nvidia-smi`, `typeperf`, `ioreg`, `pmset`, `diskutil`, NVMe and SATA SMART) are in `parse.go` and `health.go`, so their tests run anywhere.
