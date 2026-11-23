package main

// The C API the Turing Monitor Manager app links against. Implemented here,
// in the main package, because a c-archive generates its header from the main
// package's //export directives; a library package's exports do not reach it.
//
// The underlying machinery is internal/manager, which does not know about C.

/*
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>

typedef struct {
	char *serial;   // USB serial number, stable per unit
	char *model;    // e.g. `Turing 12.3" (720x1920)`
	bool running;   // is this panel driven as a display
	char *display;  // virtual display name while running, e.g. "TURZX 1"
} turzx_panel_info_t;
*/
import "C"

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/jijiechen/turing-monitor/internal/manager"
	"github.com/jijiechen/turing-monitor/internal/orient"
	"github.com/jijiechen/turing-monitor/internal/usb"
)

//export turzx_manager_version
func turzx_manager_version() *C.char {
	return C.CString("1.0.0")
}

// The process-wide manager: one app process drives one manager, the same way
// one turzx monitor process does.
var (
	mgrOnce sync.Once
	mgr     *manager.Manager
	mgrCtx  *managerContext
)

type managerContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func instance() *manager.Manager {
	mgrOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		mgrCtx = &managerContext{ctx: ctx, cancel: cancel}
		mgr = manager.New(manager.Config{Orientation: orient.Landscape}, appLog())
	})
	return mgr
}

// appLog writes the manager's diagnostics where a GUI app's user can find
// them: ~/Library/Logs/turzx-manager.log.
func appLog() *logFile {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return &logFile{path: filepath.Join(home, "Library", "Logs", "turzx-manager.log")}
}

type logFile struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return len(p), nil // nowhere to write; swallowing is better than dying
		}
		l.f = f
	}
	_, _ = l.f.Write(p)
	return len(p), nil
}

//export turzx_list_panels
func turzx_list_panels(buf *C.turzx_panel_info_t, cap C.int) C.int {
	list := instance().List()
	if buf == nil || cap <= 0 {
		// Size query: report how many panels there are without writing any.
		return C.int(len(list))
	}
	n := C.int(0)
	for i, st := range list {
		if C.int(i) >= cap {
			break
		}
		e := (*C.turzx_panel_info_t)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*C.sizeof_turzx_panel_info_t))
		e.serial = C.CString(st.Serial)
		e.model = C.CString(st.Model)
		e.running = C.bool(st.Running)
		e.display = C.CString(st.Display)
		n++
	}
	return n
}

//export turzx_start_panel
func turzx_start_panel(serial *C.char) C.int {
	s := C.GoString(serial)
	if s == "" {
		return -1
	}
	if err := instance().Start(mgrCtx.ctx, s); err != nil {
		return -2
	}
	return 0
}

//export turzx_stop_panel
func turzx_stop_panel(serial *C.char) {
	instance().Stop(C.GoString(serial))
}

//export turzx_set_brightness
func turzx_set_brightness(serial *C.char, percent C.int) C.int {
	s := C.GoString(serial)
	dev, err := usb.OpenSerial(s)
	if err != nil {
		return -1
	}
	defer dev.Close()
	if err := dev.SetBrightness(int(percent)); err != nil {
		return -2
	}
	return 0
}

//export turzx_free_string
func turzx_free_string(s *C.char) {
	C.free(unsafe.Pointer(s))
}

// main exists so this is a main package; the app's real entry point is its
// own. When linked into the app, this runs once at load and must return.
func main() {}
