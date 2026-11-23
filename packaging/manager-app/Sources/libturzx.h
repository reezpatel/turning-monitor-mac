// C API for the Turing Monitor Manager app.
//
// One manager instance per process. Panels are identified by USB serial
// number, the same identity `turzx info` prints.

#ifndef TURZX_MANAGER_CAPI_H
#define TURZX_MANAGER_CAPI_H

#include <stdbool.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// turzx_panel_info_t describes one attached panel. Strings are owned by the
// API and must be freed with turzx_free_string.
typedef struct {
	char *serial;   // USB serial number, stable per unit
	char *model;    // e.g. `Turing 12.3" (720x1920)`
	bool running;   // is this panel driven as a display
	char *display;  // virtual display name while running, e.g. "TURZX 1"
} turzx_panel_info_t;

// turzx_manager_version returns the API version; free with turzx_free_string.
const char *turzx_manager_version(void);

// turzx_list_panels fills buf with up to cap panels, returning how many were
// written. Call with buf == NULL to learn how many exist.
int turzx_list_panels(turzx_panel_info_t *buf, int cap);

// turzx_start_panel drives the named panel as an extended desktop until
// turzx_stop_panel. Returns 0 on success.
int turzx_start_panel(const char *serial);

// turzx_stop_panel stops driving the named panel.
void turzx_stop_panel(const char *serial);

// turzx_set_brightness sets one panel's backlight, 0-100. Returns 0 on
// success.
int turzx_set_brightness(const char *serial, int percent);

// turzx_free_string releases API-owned strings.
void turzx_free_string(char *s);

#ifdef __cplusplus
}
#endif

#endif /* TURZX_MANAGER_CAPI_H */

