// Command turzx drives TURZX / 图灵智显 USB panels (the 0x1cbe family) from
// macOS and Linux.
//
// The panels speak a vendor protocol over two bulk endpoints, so no kernel
// driver is required on either platform. See docs/PROTOCOL.md for the wire
// format.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jijiechen/turing-monitor/internal/capture"
	"github.com/jijiechen/turing-monitor/internal/dashboard"
	"github.com/jijiechen/turing-monitor/internal/manager"
	"github.com/jijiechen/turing-monitor/internal/media"
	"github.com/jijiechen/turing-monitor/internal/orient"
	"github.com/jijiechen/turing-monitor/internal/usb"
	"github.com/jijiechen/turing-monitor/internal/vdisplay"
)

const usage = `turzx — drive a TURZX / 图灵智显 USB display

Usage:
  turzx info                     list attached panels
  turzx sync                     handshake with the panel
  turzx clear                    blank the screen
  turzx image [--orientation NAME] <file>
                                 display a JPEG or PNG, scaled to fit
                                 NAME describes how the panel is mounted:
                                   portrait            panel as it comes (default)
                                   landscape           turned 90° anticlockwise, right edge up
                                   portrait-inverted   panel upside down
                                   landscape-inverted  turned 90° clockwise, left edge up
  turzx video [--loop] [--fps N] <file>
                                 stream an MP4 or raw Annex-B H.264 file
  turzx brightness <0-100>       set the backlight
  turzx rotate <0-3>             set the display rotation
  turzx monitor [--fps 30] [--bitrate 16000] [--orientation NAME]
                [--output NAME[,NAME...]] [--size WxH] [--serial SERIAL]...
                                 use every attached panel as an extended
                                 desktop; --serial and --output (repeatable,
                                 or comma-separated) pair up to drive only
                                 chosen panels on chosen outputs
  turzx dashboard [--interval 1s] [--orientation NAME]
                                 live system monitor on the panel
  turzx storage                  report the panel's SD card usage
  turzx extract <in.mp4> <out.h264>
                                 convert an MP4 to raw Annex-B H.264

Add --verbose to log each frame while the dashboard runs.\nRun with no arguments for this message.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "turzx: %v\n", err)
		os.Exit(1)
	}
}

// verbose enables per-frame logging for long-running commands.
var verbose bool

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	// --verbose may appear anywhere on the command line, so strip it before
	// dispatching.
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--verbose" || a == "-v" {
			verbose = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	cmd, rest := args[0], args[1:]

	// These commands look after the panel themselves: they have to survive it
	// being unplugged and plugged back in, so they open and reopen the device
	// rather than borrowing one opened here.
	switch cmd {
	case "monitor":
		opts, err := parseMonitorArgs(rest)
		if err != nil {
			return err
		}
		return cmdMonitor(context.Background(), opts)
	case "dashboard":
		opts, err := parseDashboardArgs(rest)
		if err != nil {
			return err
		}
		return cmdDashboard(context.Background(), opts)
	}

	// `info` deliberately does not claim the panel, so it works even while a
	// stream is running.
	if cmd == "info" {
		return cmdInfo()
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return nil
	}

	// These commands set one value on the panel and return. With several
	// panels attached they are applied to each in turn, since there is nothing
	// panel-specific about a brightness level.
	switch cmd {
	case "sync", "clear", "brightness", "rotate":
		return forEachPanel(func(dev *usb.Device) error {
			return runPanelCommand(cmd, rest, dev)
		})
	}

	dev, err := usb.Open()
	if err != nil {
		return err
	}
	defer dev.Close()

	switch cmd {
	case "image":
		imgOpts, err := parseImageArgs(rest)
		if err != nil {
			return err
		}
		return cmdImage(dev, imgOpts)

	case "video":
		opts, err := parseVideoArgs(rest)
		if err != nil {
			return err
		}
		return cmdVideo(dev, opts)

	case "extract":
		if len(rest) != 2 {
			return errors.New("usage: turzx extract <input.mp4> <output.h264>")
		}
		return cmdExtract(rest[0], rest[1])

	case "storage":
		s, err := dev.StorageInfo()
		if err != nil {
			return err
		}
		fmt.Printf("total %s\nused  %s\nvalid %s\n", humanBytes(s.Total), humanBytes(s.Used), humanBytes(s.Valid))
		return nil

	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// forEachPanel opens every attached panel in turn and hands it to fn. With one
// panel it behaves exactly like a single open; with several, the command
// applies to each, and the first failure is reported after the rest have had
// their turn.
func forEachPanel(fn func(*usb.Device) error) error {
	infos, err := usb.Discover()
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return usb.ErrNotFound
	}

	var firstErr error
	for _, info := range infos {
		dev, err := usb.OpenSerial(info.Serial)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := fn(dev); err != nil && firstErr == nil {
			firstErr = err
		}
		dev.Close()
	}
	return firstErr
}

// runPanelCommand dispatches the per-panel setting commands shared by
// forEachPanel.
func runPanelCommand(cmd string, rest []string, dev *usb.Device) error {
	switch cmd {
	case "sync":
		return dev.Sync()
	case "clear":
		return dev.Clear()
	case "brightness":
		if len(rest) != 1 {
			return errors.New("usage: turzx brightness <0-100>")
		}
		percent, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid brightness %q: expected a number 0-100", rest[0])
		}
		return dev.SetBrightness(percent)
	case "rotate":
		if len(rest) != 1 {
			return errors.New("usage: turzx rotate <0-3>")
		}
		rotation, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid rotation %q: expected a number 0-3", rest[0])
		}
		return dev.SetRotation(rotation)
	}
	return fmt.Errorf("unknown panel command %q", cmd)
}

func cmdInfo() error {
	infos, err := usb.Discover()
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return usb.ErrNotFound
	}
	for _, info := range infos {
		fmt.Println(info)
	}
	return nil
}

// imageOpts holds the parsed flags for the image subcommand.
type imageOpts struct {
	path        string
	orientation orient.Orientation
}

// parseImageArgs accepts: [--orientation NAME] <file>
func parseImageArgs(args []string) (imageOpts, error) {
	opts := imageOpts{orientation: orient.Portrait}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--orientation" || arg == "--rotate":
			if i+1 >= len(args) {
				return opts, errors.New("--orientation needs a value: " + orientationNames())
			}
			i++
			o, err := orient.Parse(args[i])
			if err != nil {
				return opts, err
			}
			opts.orientation = o
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			if opts.path != "" {
				return opts, errors.New("usage: turzx image [--orientation NAME] <file>")
			}
			opts.path = arg
		}
	}
	if opts.path == "" {
		return opts, errors.New("usage: turzx image [--orientation NAME] <file>")
	}
	return opts, nil
}

// orientationNames lists the accepted values, for error messages and help.
func orientationNames() string {
	names := make([]string, 0, 4)
	for _, o := range orient.All() {
		names = append(names, o.String())
	}
	return strings.Join(names, ", ")
}

func cmdImage(dev *usb.Device, opts imageOpts) error {
	data, err := os.ReadFile(opts.path)
	if err != nil {
		return err
	}
	jpeg, err := prepareImage(data, dev.Model(), opts.orientation)
	if err != nil {
		return fmt.Errorf("%s: %w", opts.path, err)
	}
	cw, ch := opts.orientation.ContentSize(dev.Portrait())
	fmt.Printf("sending %s as %d bytes of JPEG (%s, %s %dx%d)\n",
		opts.path, len(jpeg), dev.Model(), opts.orientation, cw, ch)
	return dev.ShowJPEG(jpeg)
}

// videoOpts holds the parsed flags for the video subcommand.
type videoOpts struct {
	path   string
	loop   bool
	fps    int // 0 means "derive from the file, or fall back to defaultFPS"
	fpsSet bool
}

// defaultFPS is used for raw H.264 files, which carry no timing information.
const defaultFPS = 25

// parseVideoArgs accepts: [--fps N] [--loop] <file>
func parseVideoArgs(args []string) (videoOpts, error) {
	var opts videoOpts
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--loop" || arg == "-loop":
			opts.loop = true
		case arg == "--fps":
			if i+1 >= len(args) {
				return opts, errors.New("--fps needs a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return opts, fmt.Errorf("invalid --fps %q: expected a number", args[i])
			}
			opts.fps, opts.fpsSet = n, true
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			if opts.path != "" {
				return opts, errors.New("usage: turzx video [--fps N] [--loop] <file>")
			}
			opts.path = arg
		}
	}
	if opts.path == "" {
		return opts, errors.New("usage: turzx video [--fps N] [--loop] <file>")
	}
	return opts, nil
}

func cmdVideo(dev *usb.Device, opts videoOpts) error {
	f, err := os.Open(opts.path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	isMP4 := false
	switch ext := strings.ToLower(filepath.Ext(opts.path)); ext {
	case ".mp4", ".m4v", ".mov":
		isMP4 = true
	}

	// The panel plays frames as they arrive, so it needs to be told the
	// intended rate or playback runs as fast as the USB link delivers.
	fps := opts.fps
	if fps == 0 {
		fps = defaultFPS
		if isMP4 {
			if probed, err := media.ProbeFPS(f, info.Size()); err == nil && probed >= 1 {
				fps = int(math.Round(probed))
				fmt.Printf("source is %.2f fps\n", probed)
			}
		}
	}
	if fps < 1 {
		fps = 1
	}
	if fps > 255 {
		fps = 255
	}
	fmt.Printf("playing at %d fps\n", fps)
	if err := dev.BeginPlayback(fps); err != nil {
		return err
	}

	for {
		if err := streamOnce(dev, f, info.Size(), isMP4, opts.path); err != nil {
			return err
		}
		if !opts.loop {
			break
		}
	}
	return dev.StopStream()
}

// streamOnce sends one pass over the file. For MP4 the file is demuxed to an
// Annex-B elementary stream on the fly, so nothing is buffered to disk.
func streamOnce(dev *usb.Device, f *os.File, size int64, isMP4 bool, path string) error {
	src := io.Reader(f)
	if isMP4 {
		pr, pw := io.Pipe()
		defer pr.Close()
		go func() {
			pw.CloseWithError(media.ExtractAnnexB(f, size, pw))
		}()
		src = pr
	}
	fmt.Printf("streaming %s to %s\n", filepath.Base(path), dev.Model())
	return dev.StreamH264(src)
}

// monitorOpts holds the parsed flags for the monitor subcommand.
type monitorOpts struct {
	fps     int
	bitrate int
	// serials, when set, restrict the command to the panels with these USB
	// serial numbers. Empty means every attached panel.
	serials []string
	// outputs names specific display outputs, one per panel, for platforms
	// where the virtual display has to be configured ahead of time rather
	// than created here.
	outputs []string
	// width and height size the virtual display when no panel is attached to
	// ask. A service may well start before the panel is plugged in.
	width, height int
	// orientation is how the panel is physically mounted. The virtual display
	// is created at the size the viewer sees, and frames are rotated on the
	// way to the encoder.
	orientation orient.Orientation
}

// parseMonitorArgs accepts: [--fps N] [--bitrate KBPS] [--serial S]...
// [--output NAME]... [--orientation NAME] [--size WxH]
func parseMonitorArgs(args []string) (monitorOpts, error) {
	opts := monitorOpts{fps: 30, bitrate: 16_000_000, width: 720, height: 1280, orientation: orient.Landscape}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--fps":
			if i+1 >= len(args) {
				return opts, errors.New("--fps needs a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > 60 {
				return opts, fmt.Errorf("invalid --fps %q: expected 1-60", args[i])
			}
			opts.fps = n
		case arg == "--bitrate":
			if i+1 >= len(args) {
				return opts, errors.New("--bitrate needs a value in kbit/s")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 100 {
				return opts, fmt.Errorf("invalid --bitrate %q: expected kbit/s, at least 100", args[i])
			}
			opts.bitrate = n * 1000
		case arg == "--orientation" || arg == "--rotate":
			if i+1 >= len(args) {
				return opts, errors.New("--orientation needs a value: " + orientationNames())
			}
			i++
			o, err := orient.Parse(args[i])
			if err != nil {
				return opts, err
			}
			opts.orientation = o
		case arg == "--size":
			if i+1 >= len(args) {
				return opts, errors.New("--size needs a value like 720x1280")
			}
			i++
			w, h, err := parseSize(args[i])
			if err != nil {
				return opts, err
			}
			opts.width, opts.height = w, h
		case arg == "--output":
			if i+1 >= len(args) {
				return opts, errors.New("--output needs a value, e.g. DUMMY0")
			}
			i++
			// Comma-separated names are accepted so one flag can size up the
			// whole arrangement: --output DUMMY0,DUMMY1.
			for _, name := range strings.Split(args[i], ",") {
				if name = strings.TrimSpace(name); name != "" {
					opts.outputs = append(opts.outputs, name)
				}
			}
		case arg == "--serial":
			if i+1 >= len(args) {
				return opts, errors.New("--serial needs a value, as printed by turzx info")
			}
			i++
			opts.serials = append(opts.serials, args[i])
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	return opts, nil
}

// parseSize reads a WxH dimension pair.
func parseSize(s string) (int, int, error) {
	ws, hs, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0, fmt.Errorf("invalid --size %q: expected WxH, e.g. 720x1280", s)
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(ws))
	h, err2 := strconv.Atoi(strings.TrimSpace(hs))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid --size %q: expected WxH, e.g. 720x1280", s)
	}
	return w, h, nil
}

// displayLabel names a virtual display for logging, using whichever identifier
// the platform actually provides.
func displayLabel(d *vdisplay.Display) string {
	if name := d.Name(); name != "" {
		return name
	}
	return fmt.Sprintf("%d", d.ID())
}

// cmdMonitor makes the panels real extended desktops: for each attached panel
// the manager creates a virtual display, captures it, and streams the encoded
// result to that panel. All of them run concurrently, each with its own
// supervisor, so one panel being unplugged does not disturb the others.
//
// The displays and the captures are established once and kept for the life of
// the process. Only the USB links are torn down and rebuilt when a panel is
// unplugged and plugged back in: on macOS a recreated virtual display loses
// its position in the arrangement and takes the windows that were on it with
// it, so a display that blinks out every time someone bumps the cable would be
// worse than useless.
func cmdMonitor(ctx context.Context, opts monitorOpts) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Which panels to drive: the ones named by --serial, or every attached
	// panel that reports a serial. A panel without a serial cannot be pinned
	// to its own supervisor; with others attached, driving it alongside them
	// would mean two supervisors racing for the same device.
	serials := opts.serials
	if len(serials) == 0 {
		found, err := usb.Discover()
		if err != nil {
			found = nil
		}
		for _, info := range found {
			if info.Serial != "" {
				serials = append(serials, info.Serial)
			}
		}
		// Nothing attached (or nothing pinnable): wait for one anyway, the
		// way a service started at boot relies on.
		if len(serials) == 0 {
			fmt.Println("no panel reports a serial number; waiting for one (replug the panel if this is wrong)")
			return usb.Supervise(ctx, newTimestampWriter(os.Stdout), func(dev *usb.Device) error {
				return legacyMonitor(ctx, dev, opts)
			})
		}
		if len(opts.outputs) > 0 && len(opts.outputs) != len(serials) {
			return fmt.Errorf("%d --output names for %d panels", len(opts.outputs), len(serials))
		}
	}

	log := newTimestampWriter(os.Stdout)
	mgr := manager.New(manager.Config{
		FPS:         opts.fps,
		Bitrate:     opts.bitrate,
		Orientation: opts.orientation,
		Width:       opts.width,
		Height:      opts.height,
	}, log)

	for i, serial := range serials {
		// Say which panels this run is driving, so the arrangement can be
		// checked before the displays come up.
		fmt.Fprintf(log, "driving panel %d serial %s\n", i+1, serial)
		if err := mgr.Start(ctx, serial); err != nil {
			mgr.StopAll()
			return err
		}
	}

	<-ctx.Done()
	mgr.StopAll()
	return nil
}

// legacyMonitor is the single-panel path for a panel that reports no serial
// number: one display, one capture, one supervisor on the first panel found.
// It cannot coexist with the manager (see cmdMonitor), which is why it stands
// alone.
func legacyMonitor(ctx context.Context, dev *usb.Device, opts monitorOpts) error {
	nativeW, nativeH := opts.width, opts.height
	w, h := opts.orientation.ContentSize(nativeW, nativeH)

	display, err := vdisplay.Create(vdisplay.Options{
		Name:      "TURZX",
		Width:     w,
		Height:    h,
		RefreshHz: 60,
	})
	if err != nil {
		return err
	}
	defer display.Close()
	fmt.Printf("virtual display %s at %dx%d (%s)\n", displayLabel(display), w, h, opts.orientation)

	time.Sleep(vdisplay.SettleDelay)

	originX, originY := display.Origin()
	session, err := capture.Start(capture.Options{
		DisplayID:    display.ID(),
		X:            originX,
		Y:            originY,
		Width:        w,
		Height:       h,
		QuarterTurns: int(opts.orientation),
		FPS:          opts.fps,
		Bitrate:      opts.bitrate,
		Source:       display.Source(),
	})
	if err != nil {
		return err
	}
	defer session.Close()

	if err := dev.BeginPlayback(opts.fps); err != nil {
		return err
	}
	fmt.Printf("streaming to %s\n", dev.Model())

	frames, bytes := 0, 0
	start := time.Now()
	lastReport := start

	for {
		if ctx.Err() != nil {
			_ = dev.StopStream()
			fmt.Printf("stopped after %d frames, %s\n", frames, humanBytes(int64(bytes)))
			return nil
		}

		frame, err := session.Next()
		if err != nil {
			return err
		}
		if err := dev.SendVideoFrame(frame); err != nil {
			return err
		}
		frames++
		bytes += len(frame)

		if verbose && time.Since(lastReport) >= time.Second {
			elapsed := time.Since(start).Seconds()
			fmt.Printf("frames=%6d  fps=%5.1f  avg=%7s  rate=%6.0f KB/s  queue=%d\n",
				frames, float64(frames)/elapsed,
				humanBytes(int64(bytes)/int64(max(frames, 1))),
				float64(bytes)/1024/elapsed, dev.QueueDepth())
			lastReport = time.Now()
		}
	}
}

// cmdDashboard runs the system monitor on the panel, surviving the panel being
// unplugged and plugged back in.
func cmdDashboard(ctx context.Context, opts dashboardOpts) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	run := dashboard.DefaultOptions()
	run.Interval = opts.interval
	run.Log = opts.log
	run.Orientation = opts.orientation

	fmt.Printf("dashboard refreshing every %s (%s) — press Ctrl-C to stop\n",
		opts.interval, opts.orientation)

	return usb.Supervise(ctx, newTimestampWriter(os.Stdout), func(dev *usb.Device) error {
		return dashboard.Run(ctx, dev, run)
	})
}

// dashboardOpts holds the parsed flags for the dashboard subcommand.
type dashboardOpts struct {
	interval    time.Duration
	log         io.Writer
	orientation orient.Orientation
}

// parseDashboardArgs accepts: [--interval <duration>]
func parseDashboardArgs(args []string) (dashboardOpts, error) {
	opts := dashboardOpts{interval: time.Second, orientation: orient.Portrait}
	if verbose {
		opts.log = os.Stdout
	}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--orientation" || arg == "--rotate":
			if i+1 >= len(args) {
				return opts, errors.New("--orientation needs a value: " + orientationNames())
			}
			i++
			o, err := orient.Parse(args[i])
			if err != nil {
				return opts, err
			}
			opts.orientation = o
		case arg == "--interval":
			if i+1 >= len(args) {
				return opts, errors.New("--interval needs a value, e.g. 500ms or 2s")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return opts, fmt.Errorf("invalid --interval %q: %w", args[i], err)
			}
			if d < 100*time.Millisecond {
				return opts, errors.New("--interval must be at least 100ms")
			}
			opts.interval = d
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			return opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	return opts, nil
}

// cmdExtract converts an MP4 to a raw Annex-B H.264 file, so the stream can be
// inspected or fed to other tools. It does not touch the panel.
func cmdExtract(in, out string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	w, err := os.Create(out)
	if err != nil {
		return err
	}
	defer w.Close()

	if err := media.ExtractAnnexB(f, info.Size(), w); err != nil {
		return err
	}
	st, err := w.Stat()
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s)\n", out, humanBytes(st.Size()))
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGT"[exp])
}
