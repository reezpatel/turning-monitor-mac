# Frame-rate issues: measurements and verified conclusions

> Status: **resolved**. This records every number measured on two Turing 12.3" panels
> (720×1920) on 2026-11-22/23, the verified fixes, and the rejected approaches. Do not retry
> the routes marked ❌: each one was tried on real hardware and failed in the way described.

## Baseline (one panel, 1920×720 landscape, 30 fps / 16 Mbit/s)

| Scenario | Frame rate | Data rate | Queue depth |
|---|---|---|---|
| Pure USB push (`turzx video`, pre-encoded file, no capture) | **~27 fps** | ~460 KB/s | n/a |
| Pure capture + encode (virtual display + ScreenCaptureKit + VideoToolbox, no USB) | **~25 fps** | ~10 KB/s | n/a |
| Full monitor pipeline (capture → encode → USB, serial loop, original code) | **15–17 fps** | ~100–160 KB/s | 0 |
| Full pipeline after the v2 fix (1 ms probe + per-frame queue pacing) | **~15.5 fps** | ~145 KB/s | 0, 1400 frames in 55 s, zero resets |

Early conclusions (later refined, see the final section):

- The panel's decode/USB link can do 27 fps, so **the panel is not the bottleneck**
- Capture can produce 25 fps, so **capture is not the bottleneck**
- `queue=0` throughout: the panel is always waiting for the host to feed it, so a **per-frame
  host-side cost** is the bottleneck
- The gap between 27 and 15.5 is about 25 ms of extra serial cost per frame

## Per-frame cost of a single-chunk frame (serial path)

```
session.Next()     wait for the next frame      (pure idle time when serial with USB)
d.Send(chunk):     bulk OUT write               a few ms (the data itself)
                   read the device reply        a few ms (round trip)
                   flushLocked() probe          3 ms × 5 = up to 15 ms (1 ms × 5 after v2)
waitForQueue():    CmdStreamStatus round trip   a few ms (once per frame since v2)
```

## ✅ Verified fixes (v2, merged)

1. **flushTimeout 3 ms → 1 ms.** The probe pays its full timeout when the endpoint is empty,
   so 3 ms × 5 cost up to 15 ms per frame. With 1 ms an empty endpoint costs about 1 ms.
   (`internal/usb/transport.go`)
2. **Per-frame queue pacing.** `SendVideoFrame` now polls `CmdStreamStatus` after every chunk
   (previously only between chunks of the same frame). This is what guarantees the stream
   cannot overflow the device's playback queue. (`internal/usb/ops.go`)

   v2 result: 55 s of continuous streaming, 1400+ frames, **zero session resets**.

## ❌ Rejected approaches (do not retry)

### 1. Fire-and-forget chunks (write the next chunk without waiting for the reply)
- **Symptom**: fps collapses to 0.5, frames come out oddly sized (70 KB), then
  `usb write: transfer was cancelled` and a crash loop.
- **Root cause**: the protocol is strictly request/response. The device's IN endpoint has a
  limited reply buffer; when replies are not read the endpoint backs up and the firmware
  eventually **stops accepting writes entirely**. An ack counter plus a background drain
  goroutine (100 ms ticker) did not save it either; the reply cadence is itself the throttle.
- **Collateral damage**: the repeated reopen of a crash loop can knock the panel into losing
  its serial number (`turzx info` shows an empty serial), which needs a physical replug.
  This happened several times, always for this reason.

### 2. Skipping flushLocked on the streaming path (drain only after non-streaming commands)
- **Symptom**: the very first prelude write after connecting hangs for the 5 s timeout, and
  `stop-video: usb write: transfer was cancelled` follows.
- **Root cause**: **the drain after every reply is load-bearing, not hygiene.** After some
  commands the device emits more blocks than the caller reads; if they are not drained the
  endpoint backs up until the firmware refuses writes. The panel appears frozen and only
  power-cycling recovers it.
- **Lesson**: the original 3 ms × 5 flush was both slow and necessary. The implementation
  (fixed-timeout polling) was slow; the semantics (read until empty) were necessary. v2 keeps
  the semantics and cuts the cost to 1 ms.

### 3. A pipelined frame loop without a frame copy
- **What it did**: `session.Next()` and `SendVideoFrame` each ran in their own goroutine,
  sending the previous frame while the next was captured.
- **Symptom**: fps decayed from 10 to ~1 within about 100 frames, with no session reset.
- **Root cause**: the slice returned by `session.Next()` is **only valid until the next
  call** (one internal buffer is reused). The next `Next()` overwrote bytes the send goroutine
  was still transmitting, corrupting the H.264 stream and desynchronising the panel's decoder.
- **Fix**: copy the frame before sending. See the final section; this is what shipped.

## Panel health (important)

- Under a "writes rejected → reopen repeatedly" loop the firmware can enter a state where it
  has lost its serial number: `turzx info` shows an empty serial. Serial-pinned multi-panel
  routing then degrades to single-panel mode (`cmd/turzx` `panelPlan` has this fallback);
  otherwise two supervisors would fight over the same device (`claim interface ... bad access`).
- The only recovery is a physical replug: unplug both, wait 10 seconds, plug them back in.
- Before verifying any new approach, run `./turzx info` and confirm both panels show a serial.

## Final result: the pipeline was never the limit, the test content was

The pipelined loop with the frame copy (`internal/manager/stream.go`) is what ships.

- The ~15 fps seen earlier came from the **test animation window** (an `NSView drawRect` plus a
  timer), which could only repaint at about 12 fps. ScreenCaptureKit emits frames only when
  the content changes. With a Core Animation (GPU-composited) test window:
  **two panels at once ran at 29.4 fps (target 30), queue=0, sendwait=0 ms, zero resets.**
- Per frame: rotation ~1.6 ms, encoder submit ~0.2 ms; the USB send is fully hidden by the
  pipeline.
- With real content (a playing video) the panel runs at roughly 21–27 fps, following the
  video's own frame delivery. A static desktop produces no frames, which is normal.
- Shutdown order fixed: wait for the last send to finish, then `StopStream`.
- Test-window coordinates are Cocoa (bottom-left origin); CG y=1692 is Cocoa y=-720.
- Default orientation is now landscape (1920×720 virtual display, rotated into the 720×1920
  panel).
- `TURZX_PROF=1` prints per-stage timings inside the capture callback.

## Remaining ideas (unverified, only worth it if more speed is needed)

1. **Encoder GOP**: `MaxKeyFrameInterval` equals the fps (one IDR per second). A key frame is
   ~9.5 KB against ~1 KB for a P frame; an IDR every 2 s would halve the average frame size, at
   the cost of the picture lagging for up to 2 s after a reconnect.
2. **Cheaper queue polling**: one `CmdStreamStatus` round trip per frame. Polling every N frames
   with a local token bucket would save it, but a pacing error means queue overflow and the
   crash loop; verify carefully.
3. **ScreenCaptureKit `queueDepth`** (currently 5) and the content-filter exclusion list:
   untested.

## Measurement tools

- An animated window on the virtual display (real changing content); otherwise ScreenCaptureKit
  only sends frames when something changes and the true ceiling cannot be measured. Use Core
  Animation, not `drawRect`.
- `turzx video <pre-encoded.h264>`: bypasses capture, measures the USB + decode ceiling.
- `--serial <serial>` pins a single panel to rule out interference between panels.
- The stats line of `--verbose` includes `queue=`: constant 0 means the host is the bottleneck,
  above 3 means the panel cannot keep up. `capwait` / `sendwait` split the time between waiting
  for capture and waiting for USB.
