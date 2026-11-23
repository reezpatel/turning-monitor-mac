//go:build darwin

package vdisplay

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework CoreGraphics -framework AppKit

#include <stdint.h>
#include <stdlib.h>

// Implemented in vdisplay_darwin.m.
int turzx_vd_create(const char *name, int width, int height, int refreshHz,
                    unsigned int serialNum, uint32_t *outID, void **outHandle);
void turzx_vd_destroy(void *handle);
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

// Create adds a virtual display and returns it.
//
// The caller must keep the process alive and call Close when finished; macOS
// removes the display as soon as the owning process exits.
func Create(opts Options) (*Display, error) {
	opts, err := opts.normalise()
	if err != nil {
		return nil, err
	}

	cName := C.CString(opts.Name)
	defer C.free(unsafe.Pointer(cName))

	var id C.uint32_t
	var handle unsafe.Pointer
	rc := C.turzx_vd_create(cName, C.int(opts.Width), C.int(opts.Height),
		C.int(opts.RefreshHz), C.uint(opts.Serial), &id, &handle)
	if rc != 0 {
		return nil, fmt.Errorf("create virtual display: %s", createError(rc))
	}
	return &Display{
		id:     uint32(id),
		name:   opts.Name,
		width:  opts.Width,
		height: opts.Height,
		handle: handle,
	}, nil
}

// createError explains the failure codes returned from Objective-C, which
// cannot return a Go error.
func createError(rc C.int) string {
	switch int(rc) {
	case -1:
		return "invalid dimensions"
	case -2:
		return "WindowServer refused the display descriptor " +
			"(this is private API and may have changed in this macOS release)"
	case -3:
		return "the display rejected the requested mode"
	case -4:
		return "CGVirtualDisplay is not present in this macOS release"
	default:
		return fmt.Sprintf("unknown error %d", int(rc))
	}
}

// Close removes the virtual display.
func (d *Display) Close() error {
	if d.handle != nil {
		C.turzx_vd_destroy(d.handle)
		d.handle = nil
	}
	return nil
}

// SettleDelay is how long to wait after creating the display before capturing
// it. macOS publishes a new display asynchronously, so capture started too early finds nothing.
const SettleDelay = 1500 * time.Millisecond
