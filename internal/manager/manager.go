// Package manager drives one or more panels as extended desktops, with each
// panel's pipeline (virtual display, capture, supervised USB session) started
// and stopped independently.
//
// It is the engine behind both `turzx monitor` and the Turing Monitor Manager
// menu-bar app: the CLI runs every attached panel until interrupted, while the
// app toggles individual panels on and off as the user chooses. The pieces a
// caller needs are therefore: discover what is attached, start one panel by
// serial, stop it again, and ask what state it is in.
package manager

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jijiechen/turing-monitor/internal/capture"
	"github.com/jijiechen/turing-monitor/internal/orient"
	"github.com/jijiechen/turing-monitor/internal/usb"
	"github.com/jijiechen/turing-monitor/internal/vdisplay"
)

// Config carries the settings shared by every panel a Manager drives. They are
// the same knobs `turzx monitor` exposes on its command line.
type Config struct {
	// FPS is the target frame rate. Zero means 30.
	FPS int
	// Bitrate is the target H.264 bitrate in bits per second. Zero means
	// 16 Mbit/s.
	Bitrate int
	// Orientation is how the panels are physically mounted. The virtual
	// display is created at the size the viewer sees, and frames are rotated
	// on the way to the encoder. Zero means portrait.
	Orientation orient.Orientation
	// Width and Height size the virtual display when a panel is not attached
	// to ask; a service may start before its panels are plugged in. Zero
	// means 720x1280.
	Width, Height int
	// DisplayIndex seeds the per-panel display identity (name and serial in
	// the display descriptor). Panels are numbered from it in the order they
	// are started. Zero means 1.
	DisplayIndex int
}

func (c Config) withDefaults() Config {
	if c.FPS <= 0 {
		c.FPS = 30
	}
	if c.Bitrate <= 0 {
		c.Bitrate = 16_000_000
	}
	if c.Width <= 0 || c.Height <= 0 {
		c.Width, c.Height = 720, 1280
	}
	if c.DisplayIndex <= 0 {
		c.DisplayIndex = 1
	}
	return c
}

// PanelStatus is what a Manager reports about one panel it knows about.
type PanelStatus struct {
	// Serial is the panel's USB serial number, its stable identity. It is
	// empty for a panel that did not report one; see usb.Info.
	Serial string
	// Model names the panel variant, e.g. `Turing 12.3" (720x1920)`.
	Model string
	// Running reports whether this panel's pipeline is active.
	Running bool
	// DisplaysAs reports the name of the virtual display created for the
	// panel while it runs, e.g. "TURZX 1".
	Display string
}

// Manager owns a set of panel pipelines, at most one per serial.
//
// The zero value is not usable; construct with New.
type Manager struct {
	cfg Config
	log io.Writer

	mu sync.Mutex
	// panels maps serial → running pipeline. A panel without a serial cannot
	// be keyed this way and is never driven by a Manager; Discover reports
	// it so callers can warn (turzx monitor drives it standalone instead).
	panels map[string]*pipeline
	// nextIndex hands out display identities; a stopped panel's index is
	// reused so toggling does not renumber the arrangement.
	nextIndex int
	// free holds recycled display indexes.
	free []int
}

// New creates a Manager. log receives one line per notable event, prefixed
// per panel; it may be nil to discard.
func New(cfg Config, log io.Writer) *Manager {
	return &Manager{
		cfg:    cfg.withDefaults(),
		log:    log,
		panels: make(map[string]*pipeline),
	}
}

// List reports every attached panel, marking which are running.
func (m *Manager) List() []PanelStatus {
	infos, err := usb.Discover()
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]PanelStatus, 0, len(infos))
	for _, info := range infos {
		st := PanelStatus{Serial: info.Serial, Model: info.Model.String()}
		if p, ok := m.panels[info.Serial]; ok && p.running() {
			st.Running = true
			st.Display = p.label()
		}
		out = append(out, st)
	}
	return out
}

// Start begins driving the panel with the given serial: it creates the panel's
// virtual display, starts capture, and supervises the USB link until Stop.
//
// Starting an already-running panel is a no-op. The returned error reports
// setup failures (display could not be created, capture could not start);
// after a successful Start the pipeline survives the panel being unplugged
// and replugged on its own.
func (m *Manager) Start(ctx context.Context, serial string) error {
	m.mu.Lock()
	if p, ok := m.panels[serial]; ok && p.running() {
		m.mu.Unlock()
		return nil
	}

	index := m.takeIndex()
	ctx, cancel := context.WithCancel(ctx)
	p := &pipeline{
		serial: serial,
		index:  index,
		cfg:    m.cfg,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.panels[serial] = p
	m.mu.Unlock()

	go func() {
		defer close(p.done)
		p.run(ctx, m.log)
		m.mu.Lock()
		if m.panels[serial] == p {
			delete(m.panels, serial)
			m.freeIndex(index)
		}
		m.mu.Unlock()
	}()
	return nil
}

// Stop ends the panel's pipeline and removes its virtual display.
func (m *Manager) Stop(serial string) {
	m.mu.Lock()
	p, ok := m.panels[serial]
	m.mu.Unlock()
	if !ok {
		return
	}
	p.cancel()
	<-p.done
}

// StopAll ends every running pipeline.
func (m *Manager) StopAll() {
	m.mu.Lock()
	serials := make([]string, 0, len(m.panels))
	for s := range m.panels {
		serials = append(serials, s)
	}
	m.mu.Unlock()
	for _, s := range serials {
		m.Stop(s)
	}
}

// takeIndex hands out the next display index, reusing freed ones so the
// arrangement does not renumber after a toggle. Callers hold m.mu.
func (m *Manager) takeIndex() int {
	if n := len(m.free); n > 0 {
		i := m.free[n-1]
		m.free = m.free[:n-1]
		return i
	}
	m.nextIndex++
	return m.nextIndex
}

func (m *Manager) freeIndex(i int) {
	m.free = append(m.free, i)
}

// pipeline is one panel's running display+capture+USB stack.
type pipeline struct {
	serial string
	index  int
	cfg    Config
	cancel context.CancelFunc
	done   chan struct{}
}

// label is the panel's display name, e.g. "TURZX 1".
func (p *pipeline) label() string {
	return fmt.Sprintf("TURZX %d", p.index)
}

func (p *pipeline) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// run drives the panel for the life of ctx: display, capture, and the
// supervised USB session. A panel that goes missing is waited for, not an
// error.
func (p *pipeline) run(ctx context.Context, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	tagged := newPrefixWriter(fmt.Sprintf("panel %d ", p.index), log)

	nativeW, nativeH := p.cfg.Width, p.cfg.Height

	// Prefer the resolution the panel itself reports. Discover does not claim
	// the interface, so this is safe before the supervised session opens it.
	if found, err := usb.Discover(); err == nil {
		for _, info := range found {
			if info.Serial == p.serial {
				nativeW, nativeH = info.Model.Portrait()
				break
			}
		}
	}

	// The virtual display is the size the viewer sees, which is the panel's
	// native size only when the panel is mounted the way it comes.
	w, h := p.cfg.Orientation.ContentSize(nativeW, nativeH)

	display, err := vdisplay.Create(vdisplay.Options{
		Name:      p.label(),
		Width:     w,
		Height:    h,
		RefreshHz: 60,
		Serial:    uint32(p.index),
	})
	if err != nil {
		return fmt.Errorf("panel %d: %w", p.index, err)
	}
	defer display.Close()
	fmt.Fprintf(tagged, "virtual display %s at %dx%d (%s)\n",
		p.label(), w, h, p.cfg.Orientation)

	// macOS publishes a new display asynchronously; capturing too early finds
	// nothing. Linux outputs already exist and need no wait.
	time.Sleep(vdisplay.SettleDelay)

	originX, originY := display.Origin()
	session, err := capture.Start(capture.Options{
		DisplayID:    display.ID(),
		X:            originX,
		Y:            originY,
		Width:        w,
		Height:       h,
		QuarterTurns: int(p.cfg.Orientation),
		FPS:          p.cfg.FPS,
		Bitrate:      p.cfg.Bitrate,
		Source:       display.Source(),
	})
	if err != nil {
		return fmt.Errorf("panel %d: %w", p.index, err)
	}
	defer session.Close()

	capturePath := "screen capture"
	if display.Source() != nil {
		capturePath = "display framebuffer (" + display.Source().Format().String() + ")"
	} else if err := display.SourceError(); err != nil {
		capturePath = "screen capture (framebuffer unavailable: " + err.Error() + ")"
	}
	fmt.Fprintf(tagged, "capturing %dx%d by %s, encoding %dx%d at %d fps, %d kbit/s\n",
		w, h, capturePath, nativeW, nativeH, p.cfg.FPS, p.cfg.Bitrate/1000)

	return usb.SuperviseSerial(ctx, p.serial, tagged, func(dev *usb.Device) error {
		return streamToPanel(ctx, dev, session, p.cfg.FPS, tagged)
	})
}

// prefixWriter labels every line with a prefix so several panels sharing one
// log can be told apart.
type prefixWriter struct {
	prefix string
	w      io.Writer
}

func newPrefixWriter(prefix string, w io.Writer) io.Writer {
	return &prefixWriter{prefix: prefix, w: w}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	n, err := fmt.Fprintf(p.w, "%s%s", p.prefix, b)
	if n > len(b) {
		n = len(b)
	}
	return n, err
}
