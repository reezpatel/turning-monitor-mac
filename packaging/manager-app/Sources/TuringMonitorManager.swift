// TuringMonitorManager — a menu-bar app that lists attached TURZX panels and
// toggles each one on or off as an extended display.
//
// The panel machinery lives in the Go library this app links against; this
// file is only the interface. Panels are identified by USB serial number, the
// same identity `turzx info` prints, so a panel keeps its toggle state across
// replugs even when the bus renumbers it.

import SwiftUI
import ServiceManagement
import CTurzx

/// One row's worth of panel state, refreshed from the library.
struct PanelRow: Identifiable {
    let id: String        // USB serial
    let model: String
    var running: Bool
    var display: String

    /// Short, stable label for the menu: the serial's last four characters.
    var shortLabel: String {
        String(id.suffix(4))
    }
}

/// Wrapper around the C API, kept in one place so the views stay declarative.
enum PanelBridge {
    static func list() -> [PanelRow] {
        let count = turzx_list_panels(nil, 0)
        guard count > 0 else { return [] }
        var buf = [turzx_panel_info_t](repeating: turzx_panel_info_t(), count: Int(count))
        let written = turzx_list_panels(&buf, count)
        var rows: [PanelRow] = []
        for i in 0..<Int(written) {
            defer {
                turzx_free_string(buf[i].serial)
                turzx_free_string(buf[i].model)
                turzx_free_string(buf[i].display)
            }
            let serial = buf[i].serial.map { String(cString: $0) } ?? ""
            let model = buf[i].model.map { String(cString: $0) } ?? ""
            let display = buf[i].display.map { String(cString: $0) } ?? ""
            if serial.isEmpty { continue } // cannot be pinned; not drivable from the app
            rows.append(PanelRow(id: serial, model: model, running: buf[i].running, display: display))
        }
        return rows
    }

    static func start(serial: String) {
        var s = serial
        s.withUTF8 { buf in
            buf.baseAddress.map { ptr in
                _ = turzx_start_panel(UnsafeMutablePointer<CChar>(OpaquePointer(ptr)))
            }
        }
    }

    static func stop(serial: String) {
        var s = serial
        s.withUTF8 { buf in
            buf.baseAddress.map { ptr in
                turzx_stop_panel(UnsafeMutablePointer<CChar>(OpaquePointer(ptr)))
            }
        }
    }

    static func setBrightness(serial: String, percent: Int32) {
        var s = serial
        s.withUTF8 { buf in
            buf.baseAddress.map { ptr in
                _ = turzx_set_brightness(UnsafeMutablePointer<CChar>(OpaquePointer(ptr)), percent)
            }
        }
    }
}

/// Observable state for the whole menu.
final class AppModel: ObservableObject {
    @Published var panels: [PanelRow] = []
    @Published var lastError: String?
    @Published var launchAtLogin: Bool = SMAppService.mainApp.status == .enabled

    /// Serials the user has switched on. Persisted, so a panel that was on when
    /// the app quit (or the Mac shut down) comes back on by itself.
    private var enabled: Set<String> {
        get { Set(UserDefaults.standard.stringArray(forKey: "enabledPanels") ?? []) }
        set { UserDefaults.standard.set(Array(newValue).sorted(), forKey: "enabledPanels") }
    }
    /// Last automatic start attempt per serial, so a failing start (missing
    /// permission, say) is retried slowly rather than every poll.
    private var lastAttempt: [String: Date] = [:]
    private var timer: Timer?

    init() {
        refresh()
        // Panels appear and disappear as they are plugged in; a short poll
        // keeps the menu honest without any USB work on the timer itself
        // (turzx_list_panels only enumerates, it claims nothing).
        timer = Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
            self?.refresh()
        }
    }

    deinit { timer?.invalidate() }

    func refresh() {
        panels = PanelBridge.list()
        autoStart()
    }

    /// Starts every panel that is switched on but not running, as soon as it
    /// is present.
    private func autoStart() {
        let want = enabled
        for panel in panels where want.contains(panel.id) && !panel.running {
            if let last = lastAttempt[panel.id], Date().timeIntervalSince(last) < 30 { continue }
            lastAttempt[panel.id] = Date()
            PanelBridge.start(serial: panel.id)
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.8) { [weak self] in
                self?.panels = PanelBridge.list()
            }
        }
    }

    func toggle(_ panel: PanelRow) {
        if panel.running {
            enabled.remove(panel.id)
            PanelBridge.stop(serial: panel.id)
        } else {
            enabled.insert(panel.id)
            lastAttempt[panel.id] = Date()
            PanelBridge.start(serial: panel.id)
        }
        // Give the pipeline a beat to come up, then reflect reality.
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.8) { [weak self] in
            self?.panels = PanelBridge.list()
        }
    }

    func setLaunchAtLogin(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() }
            else { try SMAppService.mainApp.unregister() }
        } catch {
            lastError = error.localizedDescription
        }
        launchAtLogin = SMAppService.mainApp.status == .enabled
    }
}

/// The menu-bar scene.
@main
struct TuringMonitorManagerApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        // A normal window with the same controls. The menu bar icon can be
        // pushed off-screen when the bar is crowded (or hidden by the notch),
        // and the app must stay reachable then: it opens at launch and again
        // whenever the app is re-opened from Finder.
        Window("Turing Monitor Manager", id: "main") {
            MenuContent(model: model)
                .padding(12)
                .frame(width: 320)
                .onAppear { NSApp.activate(ignoringOtherApps: true) }
        }
        .windowResizability(.contentSize)

        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Image(systemName: model.panels.contains(where: \.running)
                  ? "display.trianglebadge.exclamationmark"
                  : "display")
        }
        .menuBarExtraStyle(.window)
    }
}

/// The dropdown window's content: one section per panel, then housekeeping.
struct MenuContent: View {
    @ObservedObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if model.panels.isEmpty {
                Text("No TURZX panels found")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                Text("Plug a panel in — it appears here.")
                    .font(.caption)
                    .foregroundStyle(.tertiary)
            } else {
                ForEach(model.panels) { panel in
                    PanelToggle(panel: panel) { model.toggle(panel) }
                }
            }

            Divider()
            Toggle("Open at login", isOn: Binding(
                get: { model.launchAtLogin },
                set: { model.setLaunchAtLogin($0) }
            ))
            if let err = model.lastError {
                Text(err).font(.caption).foregroundStyle(.red)
            }
            Button("Refresh") { model.refresh() }
            Button("Quit") { NSApplication.shared.terminate(nil) }
                .keyboardShortcut("q")
        }
        .padding(10)
        .frame(minWidth: 260)
    }
}

/// One panel's toggle row with its brightness slider.
struct PanelToggle: View {
    let panel: PanelRow
    let onToggle: () -> Void
    @State private var brightness: Double = 80

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle(isOn: Binding(
                get: { panel.running },
                set: { _ in onToggle() }
            )) {
                VStack(alignment: .leading, spacing: 1) {
                    Text(panel.model)
                        .font(.body.weight(.medium))
                    Text(panel.running
                         ? "\(panel.display) · serial …\(panel.shortLabel)"
                         : "serial …\(panel.shortLabel)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            .toggleStyle(.switch)

            HStack(spacing: 6) {
                Image(systemName: "sun.min")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Slider(value: $brightness, in: 0...100, step: 5) { editing in
                    if !editing {
                        PanelBridge.setBrightness(serial: panel.id, percent: Int32(brightness))
                    }
                }
                .controlSize(.mini)
                Text("\(Int(brightness))")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
                    .frame(width: 24, alignment: .trailing)
            }
            .padding(.leading, 18)
        }
        .padding(.vertical, 2)
    }
}

