# turning-monitor-mac

> [!NOTE]
> **This is a fork of [jijiechen/turing-monitor](https://github.com/jijiechen/turing-monitor)**
> by Jay Chen, who reverse-engineered the panel protocol and wrote the original USB, video,
> dashboard and virtual-display code. All of that work is theirs and is used here under the
> MIT license. This fork adds multi-panel support, performance work, and a macOS menu-bar
> manager app (see [What's new in this fork](#whats-new-in-this-fork)).

> [!WARNING]
> **This is an unofficial, third-party implementation, not affiliated with TURZX (图灵智显),
> its companies or its resellers.** It is independently developed replacement software, not
> the vendor's original software, and it talks to hardware directly. Use it at your own risk.
>
> **Please do not file issues here about the vendor's software or the panel hardware itself:**
> - For the panel or the official software (TURZX.exe etc.), use the
>   [official forum](http://discuz.turzx.com/)
> - For other brands of panel, contact your seller
>
> Also note that **the Linux code has not been verified on real hardware** (see
> [Status](#status)).
>
> **The software is provided "as is", without warranty of any kind.** See the
> [disclaimer](#disclaimer).

Drive **TURZX (图灵智显 / Turing Smart Screen)** USB panels from **macOS** and **Linux**
with no kernel driver, no signing and no DKMS.

The vendor only ships Windows software, and its driver targets USB ID `1A86:AD10..AD13`,
which a panel in its normal "LCD mode" does not present, so the panel is not recognised on
macOS or Linux at all. This project talks to the panel from user space; both platforms only
need libusb.

What you can do with it:

- **Show images**: any JPEG or PNG, scaled to the panel
- **Play video**: MP4 or raw H.264
- **System-monitor dashboard**: CPU, memory, network, disk, temperature
- **Use it as a second display**: drag real windows onto it

## What's new in this fork

- **Several panels at once.** `turzx monitor` finds every attached panel and drives each one
  as its own virtual display (`TURZX 1`, `TURZX 2`, ...). Panels are pinned by USB **serial
  number**, because bus addresses swap between replugs. `--serial <s>` limits it to one panel.
- **Landscape by default.** The virtual display is 1920x720 and is rotated into the panel's
  native 720x1920 frame. Use
  `--orientation portrait|landscape|portrait-inverted|landscape-inverted` to change it.
- **Pipelined streaming.** Encoding the next frame overlaps with the USB send of the previous
  one. Measured on two panels at once: about 29 fps against a 30 fps target with animated
  content. Capture only produces frames when the screen changes, so a static desktop shows a
  low fps.
- **Per-second stats.** `turzx monitor --verbose` prints `fps`, `queue`, `capwait` (time
  waiting for capture) and `sendwait` (time waiting for USB). `TURZX_PROF=1` adds per-stage
  timings inside the capture callback.
- **Turing Monitor Manager** (macOS, Apple Silicon): a small SwiftUI app. It lists attached
  panels, toggles each one on or off as a display, sets brightness, remembers which panels
  are on and starts them automatically (also when a panel is plugged in later), and can open
  at login.

### Manager app

Download the zip from [Releases](../../releases), or build it yourself:

```sh
brew install libusb
./packaging/manager-app/build-app.sh
open "packaging/manager-app/Turing Monitor Manager.app"
```

Notes:

- The app is **not notarized**. After downloading, right-click > Open, or run
  `xattr -dr com.apple.quarantine "Turing Monitor Manager.app"`.
- It needs Screen Recording permission: System Settings > Privacy & Security > Screen
  Recording, enable *Turing Monitor Manager*, then reopen the app. The app is ad-hoc signed,
  so a rebuild or a new download can reset the permission.
- A window opens at launch; the menu-bar icon may be hidden on a crowded menu bar.
- Log with the live fps lines: `tail -f ~/Library/Logs/turzx-manager.log`
- Do not run the app and `turzx monitor` at the same time; they would fight over the panels.
- Panels whose serial cannot be read (it can be lost after USB crash loops) are hidden. Unplug
  all panels, wait 10 seconds and plug them back in.
- The Go side is built as a C archive from `cmd/turzxlib` (needs `CC=clang`).

## Supported models

| USB ID | Model | Resolution |
|---|---|---|
| `1cbe:0050` | Turing 5.2" | 720×1280 |
| `1cbe:0028` | Turing 2.8" round | 480×480 |
| `1cbe:0046` | Turing 4.6" | 320×960 |
| `1cbe:0080` | Turing 8.0" | 800×1280 |
| `1cbe:0088` | Turing 8.8" | 480×1920 |
| `1cbe:0092` | Turing 9.2" | 462×1920 |
| `1cbe:0123` | Turing 12.3" | 720×1920 |

Run `turzx info` to see what your panel reports. If it is not listed, add a row to the model
table in `internal/proto/model.go`.

## Install

### macOS

```sh
brew install libusb
git clone https://github.com/reezpatel/turning-monitor-mac
cd turning-monitor-mac
PKG_CONFIG_PATH="$(brew --prefix)/lib/pkgconfig" go build -o turzx ./cmd/turzx
```

Requires Go 1.24 or newer. If you would rather not install Go, download a prebuilt binary
from [Releases](../../releases). The macOS build is unsigned, so on first run use
**right-click > Open**, or:

```sh
xattr -d com.apple.quarantine ./turzx
```

### Ubuntu / Debian

```sh
sudo apt install libusb-1.0-0-dev
sudo apt install ffmpeg          # only needed by turzx monitor
git clone https://github.com/reezpatel/turning-monitor-mac
cd turning-monitor-mac
go build -o turzx ./cmd/turzx
```

Then let your user reach the panel without `sudo`:

```sh
sudo cp packaging/linux/99-turzx.rules /etc/udev/rules.d/
sudo udevadm control --reload-rules && sudo udevadm trigger
```

Unplug the panel and plug it back in. **If you skip this step every command fails with
`libusb: bad access`.**

## Usage

First check that the panel is found:

```sh
./turzx info
# Turing 5.2" (720x1280)  serial 0123456789abcdef  bus 2 addr 1
```

### Show an image

```sh
./turzx image ~/Pictures/photo.jpg
```

Any size works: the image is scaled to fit and padded with black. The panel does no scaling
of its own; all fitting happens on the host.

### Play a video

```sh
./turzx video ~/Movies/clip.mp4          # MP4 is demuxed on the fly
./turzx video clip.h264 --loop           # raw Annex-B H.264, looped
./turzx video clip.mp4 --fps 24          # set the frame rate by hand
```

MP4 parsing is built in, so **video playback does not need ffmpeg**. Pacing is controlled by
the panel from its own buffer, so the whole clip is pushed in far less than its running time
and then plays on; this is normal.

### System-monitor dashboard

```sh
./turzx dashboard                        # refresh every second
./turzx dashboard --interval 500ms
```

Shows CPU, load, memory, swap, network throughput, disk usage and temperature. Stop with
Ctrl-C.

Temperature is not shown on macOS: on Apple Silicon it can only be read through the SMC, which
an unsigned command-line tool cannot access. The layout adapts and gives the space to the disk
panel.

### Second display

```sh
./turzx monitor                  # 30 fps, 16 Mbit/s, every panel found
./turzx monitor --serial <s>     # one panel only
./turzx monitor --bitrate 24000  # sharper, if the panel keeps up
./turzx monitor --fps 24         # lower frame rate, if it does not
```

Linux needs extra setup and macOS needs a permission; see
**[Extended desktop](#extended-desktop)**.

### Other commands

```sh
./turzx brightness 60     # backlight, 0-100
./turzx rotate 1          # 0-3, for a panel mounted on its side
./turzx clear             # clear the screen
./turzx sync              # handshake, also prints the firmware version
./turzx storage           # SD card usage (if a card is inserted)
./turzx extract in.mp4 out.h264   # convert a file, no panel needed
```

## Extended desktop

`turzx monitor` creates a display the size of the panel, captures it, encodes it as H.264 and
streams it, so the panel becomes a real second display you can drag windows onto.

### macOS

You need to grant permission once: **System Settings > Privacy & Security > Screen
Recording**, tick your terminal, then **restart the terminal**. macOS prompts on first run;
without permission, capture fails with a clear message.

The virtual display is created through the private CoreGraphics API `CGVirtualDisplay`, the
same mechanism used by DisplayLink, BetterDisplay and DeskPad. Two things to know:

- It is an **undocumented API**, and any macOS update may break it
- For that reason the app cannot be published on the Mac App Store

### Linux

Two prerequisites:

**1. An X11 session.** Choose "Ubuntu on Xorg" at login. Wayland is not supported: its
compositors offer no way to create a virtual output, and screen capture goes through a portal
instead of the X screen.

**2. An existing virtual output.** User-space programs cannot create a display on Linux, so
one has to be set up first. Either install evdi:

```sh
sudo apt install evdi-dkms && sudo modprobe evdi
```

or add a dummy device to `xorg.conf`:

```
Section "Device"
  Identifier "Virtual"
  Driver     "dummy"
  VideoRam   32768
  Option     "ConnectedMonitor" "DP-1"
EndSection
```

Then run `turzx monitor`. It picks an output automatically, preferring one that already
matches the panel size. If there are several, choose with `--output <name>`; names come from
`xrandr --query`. If no suitable output exists it lists what is available and tells you how to
configure one.

### Tuning

The defaults target desktop content on USB 2.0. If moving content looks blurry, raise
`--bitrate`; if it stutters, lower `--fps`. Note that a still picture produces almost no USB
traffic (the panel keeps showing the last frame), so a low frame rate when nothing is moving is
normal.

## Running unattended

`dashboard` and `monitor` **handle the panel being unplugged themselves**: they do not exit,
they wait, and when the panel comes back they reconnect and replay the streaming prelude (the
device reset, so it must start over).

That makes them safe to leave to a service manager. Templates are provided for both platforms.

**Linux (systemd user service)**

```sh
mkdir -p ~/.config/systemd/user
cp packaging/linux/turzx.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now turzx
journalctl --user -u turzx -f        # follow the log
```

**macOS (LaunchAgent)**

```sh
cp packaging/macos/com.jijiechen.turzx.plist ~/Library/LaunchAgents/
# first edit the binary path in the plist to the real location
launchctl load ~/Library/LaunchAgents/com.jijiechen.turzx.plist
tail -f /tmp/turzx.log
```

Both templates run `dashboard` by default; change it to `monitor` for the extended desktop.
On macOS the [Manager app](#manager-app) does the same job with a UI.

**Points to note:**

- The service managers' `Restart` and `KeepAlive` are for **real crashes** only. A missing
  panel is not a fault, the program waits by itself, so there is no crash-restart loop.
- While waiting, a notice is printed only once every 60 seconds, so logs do not flood.
- **On macOS do not rely on crash-restart**: the virtual display belongs to the process, so it
  disappears when the process dies and macOS rearranges windows and displays on restart.
  In-process reconnection exists to avoid exactly that.
- The first `monitor` run needs the Screen Recording permission; with a LaunchAgent, run it
  once by hand in a terminal to grant it.

## Troubleshooting

**`no TURZX display found`**
The panel is not plugged in, or it is in desktop mode. Check with `lsusb` on Linux or System
Information > USB on macOS and look for a `1cbe:` device. If you see `1a86:ad1x`, the panel is
in desktop mode, which this project does not support.

**`libusb: bad access` / `claim interface failed`**
On Linux the udev rule is missing. On either system another `turzx` process may still be
running and holding the panel; only one process may use it at a time.

**`claim interface 0` fails right after killing the previous process**
The OS needs a moment to release the interface. Wait a few seconds and retry.

**Extended desktop reports `Screen Recording permission is not granted`**
See the macOS section above. After granting it, the terminal must be restarted.

**Moving content looks blurry or smeared**
Raise `--bitrate`. If it is still blurry, note that the panel's hardware decoder only supports
H.264 4:2:0, so small coloured text gets slight colour fringes; that cannot be tuned.

**The panel went black, but every command succeeds**
Brightness was written as 0. In `cmd 125` (save settings) **any field that is not set is
written as 0**, and `[8]` is brightness; it also **only takes effect after a restart**, so the
problem is not visible at the time. Recovery:

```sh
./turzx brightness 100     # cmd 14, takes effect immediately
# if still wrong, write the whole settings block back to sane values and restart the device
```

See the warning chapter in [docs/PROTOCOL.md](docs/PROTOCOL.md) for details.

**Video plays noticeably too fast**
Pacing is controlled by the panel from its own buffer. If it ignores the requested frame rate,
set it explicitly with `--fps`.

**A panel shows an empty serial / is missing from the app**
Repeated crash loops can make the firmware lose its serial number. Unplug every panel, wait 10
seconds and plug them back in.

## Status

| | macOS | Linux |
|---|---|---|
| Images, video, brightness, rotation | ✅ verified | ⚠️ unverified |
| System-monitor dashboard | ✅ verified | ⚠️ unverified |
| Extended desktop | ✅ verified | ⚠️ unverified |
| Multiple panels, Manager app | ✅ verified (two 12.3" panels) | n/a |

macOS was verified on real panels (Apple Silicon, macOS 26). **The Linux code is complete but
has never run on real hardware**; expect things to need fixing, and feedback is welcome.

## How it works

In LCD mode the panel exposes a vendor-specific USB interface with two bulk endpoints
(`0x01` OUT, `0x81` IN). Every message is a 512-byte block: a 500-byte command packet
encrypted with DES-CBC, optionally followed by a raw payload such as a JPEG image or a chunk of
H.264 data.

The full wire format, including the golden test vectors that pin down the crypto
implementation, is in **[docs/PROTOCOL.md](docs/PROTOCOL.md)**.

## Layout

```
cmd/turzx/              command-line entry point
cmd/turzxlib/           C API (c-archive) used by the macOS Manager app
internal/manager/       multi-panel engine: per-panel Start/Stop/List
internal/proto/         protocol layer: crypto, framing, commands (pure Go)
internal/usb/           USB transport and panel operations
internal/media/         MP4 -> Annex-B H.264 demuxer (pure Go, no ffmpeg)
internal/metrics/       system metrics, per platform
internal/dashboard/     renders metrics into a picture and pushes it
internal/vdisplay/      creates or finds a virtual display
internal/capture/       captures a display and encodes it as H.264
packaging/manager-app/  SwiftUI menu-bar Manager app
docs/PROTOCOL.md        protocol reference
```

## Development

```sh
go test ./...
```

Test the MP4 demuxer with a real file:

```sh
TURZX_TEST_MP4=/path/to/video.mp4 go test ./internal/media/ -v
```

## Notes and limitations

* The panel has **two USB modes**. This project targets **LCD mode** (`1cbe:00xx`), the mode
  the panel boots into. The vendor's Windows driver uses **desktop mode** (`1a86:AD1x`), a
  different protocol that is not implemented here.
* A single USB transfer must stay under 1 MiB or the device times out; larger payloads are
  split automatically.
* Playing files from the panel's own SD card is implemented at the protocol level but needs a
  card and is unverified.
* This is an **unofficial third-party implementation**, unrelated to TURZX.

## Credits

- **[jijiechen/turing-monitor](https://github.com/jijiechen/turing-monitor)**, the original
  project this repository is forked from.
- The protocol was cross-checked against
  [`turing-smart-screen-python`](https://github.com/mathoudebine/turing-smart-screen-python),
  whose reference implementation for this device family was very valuable.

## Disclaimer

**This software is not an official driver. It is provided "as is", without warranty of any
kind, express or implied.**

The protocol it speaks was implemented independently and has not been published or endorsed by
the vendor. Although it was verified on real devices, different hardware batches and firmware
versions may still differ.

**Using this software may cause the panel to misbehave, lose data, or even be damaged, through
limitations of the implementation, operator error or differences between individual devices.
You bear all such risk and consequences**; the authors and contributors accept no liability for
any direct or indirect damage.

If you are unsure about your device:

- Prefer the vendor's official software
- Run `turzx info` first to confirm the model before trying other commands
- Use read-only, display-only commands such as `turzx dashboard`, which carry the least risk

The full legal terms are in [LICENSE](LICENSE) (the warranty disclaimer of the MIT license).

## License

MIT, see [LICENSE](LICENSE). Original work copyright (c) 2026 Jay Chen.
