// rushbin for Mac: a window and a menu bar item around the engine (rushbin-engine), which does the work
// and serves the interface on 127.0.0.1 to this window only. Sparkle keeps it up to date. Its own few words
// are in macos/<language>.lproj (English, the keys, when none fits); the page has its own (web/i18n).
import AppKit
import IOKit.ps
import Network
import Sparkle
import WebKit

final class AppDelegate: NSObject, NSApplicationDelegate, WKUIDelegate, WKNavigationDelegate, WKScriptMessageHandler,
    NSMenuDelegate, NSMenuItemValidation, SPUUpdaterDelegate {
    var engine: Process?
    var engineInput = Pipe() // kept open: the engine quits when it closes, so it never outlives the app
    var window: NSWindow!
    var web: WKWebView!
    var status: NSStatusItem!
    var url: URL?
    let net = NWPathMonitor()
    var metered = false
    var volumes: FSEventStreamRef?
    var updates: SPUStandardUpdaterController!
    var feed: String? // the appcast of the engine's update site; nil: no update checks at all
    var links: [URL] = [] // rushbin://connect links from a team site, waiting for the page
    var pageReady = false
    var clipSelected = false // the page has a clip selected or open, and no text field focused: the Clip menu works
    var work = Work() // what rushbin is doing and the connected drives, for the menu bar item (askWork)

    // Sparkle asks the engine's update site (its team's, or Artide's only if allowed), checks in the background
    // and downloads by itself (Info.plist), and installs when rushbin quits.
    func feedURLString(for updater: SPUUpdater) -> String? { feed }

    func updater(_ updater: SPUUpdater, mayPerform updateCheck: SPUUpdateCheck) throws {
        if feed == nil { throw NSError(domain: "rushbin", code: 1, userInfo: [NSLocalizedDescriptionKey: "No update site: not allowed to ask Artide's site, and in no team."]) }
    }

    func applicationDidFinishLaunching(_ note: Notification) {
        updates = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: self, userDriverDelegate: nil)
        buildMenu()
        let config = WKWebViewConfiguration()
        config.userContentController.add(self, name: "rushbin") // the page says whether a clip is selected (tellApp)
        web = WKWebView(frame: .zero, configuration: config)
        web.pageZoom = UserDefaults.standard.object(forKey: "zoom") as? CGFloat ?? 1
        web.uiDelegate = self
        web.navigationDelegate = self
        web.setValue(false, forKey: "drawsBackground") // dark window while the engine starts, not a white flash
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1320, height: 860),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "rushbin"
        window.titleVisibility = .hidden // the page shows the name
        window.titlebarAppearsTransparent = true
        window.backgroundColor = NSColor(red: 0x12 / 255.0, green: 0x12 / 255.0, blue: 0x14 / 255.0, alpha: 1) // the page's --bg, never pure black
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 720, height: 480)
        window.contentView = web
        window.setFrameAutosaveName("main")
        if !window.setFrameUsingName("main") { window.center() }
        window.makeKeyAndOrderFront(nil)

        status = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        status.button?.image = icon(nil)
        status.menu = NSMenu()
        status.menu?.delegate = self // filled each time it opens (menuNeedsUpdate)
        Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { [weak self] _ in self?.askWork() }

        startEngine()
    }

    func startEngine() {
        guard let exe = Bundle.main.url(forAuxiliaryExecutable: "rushbin-engine") else { return fail(NSLocalizedString("Part of rushbin is missing: download the app again.", comment: "")) }
        let p = Process()
        p.executableURL = exe
        p.arguments = ["app"]
        // Bundled ffmpeg comes first (next to the engine); Homebrew only helps development builds.
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin"
        p.environment = env
        let out = Pipe()
        p.standardInput = engineInput
        p.standardOutput = out
        p.terminationHandler = { [weak self] proc in
            DispatchQueue.main.async { if NSApp.isRunning, proc.terminationReason != .exit || proc.terminationStatus != 0 { self?.fail(String(format: NSLocalizedString("rushbin stopped (code %d). Quit and reopen it.", comment: ""), proc.terminationStatus)) } }
        }
        do { try p.run() } catch { return fail(String(format: NSLocalizedString("rushbin could not start: %@", comment: ""), error.localizedDescription)) }
        engine = p
        watchConditions()
        watchVolumes()
        DispatchQueue.global().async { // one line at a time, until the engine stops
            var line = Data()
            while true {
                let b = out.fileHandleForReading.readData(ofLength: 1)
                if b.isEmpty { break }
                if b != Data([0x0a]) { line.append(b); continue }
                let s = String(decoding: line, as: UTF8.self)
                line = Data()
                DispatchQueue.main.async { self.engineSaid(s) }
            }
            DispatchQueue.main.async { if self.url == nil { self.fail(NSLocalizedString("rushbin did not answer.", comment: "")) } }
        }
    }

    // "LISTEN <url>" once: the page. "feed <url>" or "feed" alone, at start and whenever it changes: where to
    // ask for new versions, at once when it changes.
    func engineSaid(_ s: String) {
        if s.hasPrefix("LISTEN "), url == nil, let u = URL(string: String(s.dropFirst(7))) {
            url = u
            web.load(URLRequest(url: u))
        } else if s == "feed" || s.hasPrefix("feed ") {
            let f = s == "feed" ? nil : String(s.dropFirst(5))
            if f == feed { return }
            feed = f
            if f != nil { updates.updater.checkForUpdatesInBackground() }
        }
    }

    // The engine pauses its heavy work on battery, in Low Power Mode, when the Mac is hot, and the copies for
    // the team on a metered connection (iPhone hotspot, Low Data Mode). It reads them from these lines.
    func watchConditions() {
        net.pathUpdateHandler = { [weak self] path in
            DispatchQueue.main.async { self?.metered = path.isExpensive || path.isConstrained; self?.sendConditions() }
        }
        net.start(queue: .global(qos: .utility))
        for name in [Notification.Name.NSProcessInfoPowerStateDidChange, ProcessInfo.thermalStateDidChangeNotification] {
            NotificationCenter.default.addObserver(forName: name, object: nil, queue: .main) { [weak self] _ in self?.sendConditions() }
        }
        Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in self?.sendConditions() } // battery
        sendConditions()
    }

    func sendConditions() {
        let source = IOPSGetProvidingPowerSourceType(IOPSCopyPowerSourcesInfo()?.takeRetainedValue())?.takeUnretainedValue() as String?
        let info = ProcessInfo.processInfo
        let on = { (b: Bool) in b ? 1 : 0 }
        let line = "conditions battery=\(on(source == kIOPSBatteryPowerValue)) lowpower=\(on(info.isLowPowerModeEnabled)) " +
            "hot=\(on(info.thermalState.rawValue >= ProcessInfo.ThermalState.serious.rawValue)) metered=\(on(metered))\n"
        try? engineInput.fileHandleForWriting.write(contentsOf: Data(line.utf8))
    }

    // Files copied onto a connected drive: FSEvents names the folders that changed under /Volumes, every 10 s
    // at most, and the engine scans the drive holding them again. On the main queue, like the conditions, so
    // the lines never interleave.
    func watchVolumes() {
        var ctx = FSEventStreamContext(version: 0, info: Unmanaged.passUnretained(self).toOpaque(), retain: nil, release: nil, copyDescription: nil)
        let callback: FSEventStreamCallback = { _, info, n, paths, _, _ in
            guard let info else { return }
            let me = Unmanaged<AppDelegate>.fromOpaque(info).takeUnretainedValue()
            let ps = paths.assumingMemoryBound(to: UnsafePointer<CChar>.self)
            let lines = (0..<n).map { "changed " + String(cString: ps[$0]) + "\n" }.joined()
            try? me.engineInput.fileHandleForWriting.write(contentsOf: Data(lines.utf8))
        }
        guard let s = FSEventStreamCreate(nil, callback, &ctx, ["/Volumes"] as CFArray,
                                          FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 10,
                                          FSEventStreamCreateFlags(kFSEventStreamCreateFlagNone)) else { return }
        FSEventStreamSetDispatchQueue(s, .main)
        FSEventStreamStart(s)
        volumes = s
    }

    func fail(_ message: String) {
        let a = NSAlert()
        a.messageText = "rushbin"
        a.informativeText = message
        a.runModal()
    }

    @objc func show() {
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
    }

    // rushbin://connect?site=...&code=... (Info.plist), from "Open in rushbin" on a team site: the page shows the site
    // and asks before it connects (a link alone never connects, it may come from anyone), then the engine trades
    // the code for this Mac's token (team.go, connect).
    func application(_ application: NSApplication, open urls: [URL]) {
        links += urls.filter { $0.scheme == "rushbin" }
        if window != nil { show() } // a link that launches the app comes before the window
        deliverLinks()
    }

    func deliverLinks() {
        guard pageReady else { return }
        for u in links {
            guard let arg = try? JSONSerialization.data(withJSONObject: [u.absoluteString]), let list = String(data: arg, encoding: .utf8) else { continue }
            web.evaluateJavaScript("window.rushbinOpen && rushbinOpen(\(list)[0])")
        }
        links = []
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        pageReady = true
        deliverLinks()
    }

    // Closing the window keeps rushbin running in the menu bar, so drives are still cataloged when connected.
    func applicationShouldTerminateAfterLastWindowClosed(_ app: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ app: NSApplication, hasVisibleWindows: Bool) -> Bool {
        show()
        return true
    }

    func applicationWillTerminate(_ note: Notification) {
        try? engineInput.fileHandleForWriting.close()
        engine?.terminate()
    }

    // The page asks for a drive name with prompt().
    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (String?) -> Void) {
        let a = NSAlert()
        a.messageText = prompt
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 260, height: 24))
        field.stringValue = defaultText ?? ""
        a.accessoryView = field
        a.addButton(withTitle: NSLocalizedString("OK", comment: ""))
        a.addButton(withTitle: NSLocalizedString("Cancel", comment: ""))
        a.window.initialFirstResponder = field
        completionHandler(a.runModal() == .alertFirstButtonReturn ? field.stringValue : nil)
    }

    // The page asks to confirm with confirm(). Without this, WebKit answers Cancel by itself.
    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: NSLocalizedString("OK", comment: ""))
        a.addButton(withTitle: NSLocalizedString("Cancel", comment: ""))
        completionHandler(a.runModal() == .alertFirstButtonReturn)
    }

    func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
        clipSelected = (message.body as? [String: Any])?["clip"] as? Bool ?? false
    }

    // The menu bar item: what rushbin is doing, with a ring for how far it is, and the connected drives, each
    // to eject unless rushbin is reading it. The page knows both, in its own words (rushbinWork).
    struct Work { var text = ""; var pct: Int?; var drives: [(name: String, mount: String, busy: Bool)] = [] }

    func askWork() {
        guard pageReady else { return }
        web.callAsyncJavaScript("return window.rushbinWork ? await rushbinWork() : null", arguments: [:], in: nil, in: .page) { [weak self] r in
            guard let self, case .success(let v) = r, let d = v as? [String: Any] else { return }
            let drives = (d["drives"] as? [[String: Any]] ?? []).map { (name: $0["name"] as? String ?? "", mount: $0["mount"] as? String ?? "", busy: $0["busy"] as? Bool ?? false) }
            self.work = Work(text: d["text"] as? String ?? "", pct: (d["pct"] as? NSNumber)?.intValue, drives: drives)
            self.status.button?.image = self.icon(self.work.text.isEmpty ? nil : self.work.pct ?? -1)
        }
    }

    // The film stack when idle, filled while working, a ring when how far is known. Template: the system colors it.
    func icon(_ pct: Int?) -> NSImage? {
        guard let pct else { return NSImage(systemSymbolName: "film.stack", accessibilityDescription: "rushbin") }
        if pct < 0 { return NSImage(systemSymbolName: "film.stack.fill", accessibilityDescription: "rushbin") }
        let img = NSImage(size: NSSize(width: 18, height: 18), flipped: false) { r in
            let c = NSPoint(x: r.midX, y: r.midY)
            let track = NSBezierPath()
            track.appendArc(withCenter: c, radius: 7, startAngle: 0, endAngle: 360)
            track.lineWidth = 2
            NSColor.labelColor.withAlphaComponent(0.3).setStroke()
            track.stroke()
            let arc = NSBezierPath()
            arc.appendArc(withCenter: c, radius: 7, startAngle: 90, endAngle: 90 - 360 * CGFloat(pct) / 100, clockwise: true)
            arc.lineWidth = 2
            NSColor.labelColor.setStroke()
            arc.stroke()
            return true
        }
        img.isTemplate = true
        img.accessibilityDescription = "rushbin \(pct)%"
        return img
    }

    func menuNeedsUpdate(_ menu: NSMenu) {
        guard menu === status.menu else { return }
        menu.removeAllItems()
        if !work.text.isEmpty {
            menu.addItem(withTitle: work.pct.map { "\(work.text), \($0)%" } ?? work.text, action: nil, keyEquivalent: "") // no action: shown, not clickable
            menu.addItem(.separator())
        }
        for d in work.drives {
            if d.busy {
                menu.addItem(withTitle: String(format: NSLocalizedString("%@: rushbin is reading it", comment: ""), d.name), action: nil, keyEquivalent: "")
            } else {
                let item = menu.addItem(withTitle: String(format: NSLocalizedString("Eject %@", comment: ""), d.name), action: #selector(eject(_:)), keyEquivalent: "")
                item.target = self
                item.representedObject = d.mount
            }
        }
        if !work.drives.isEmpty { menu.addItem(.separator()) }
        menu.addItem(withTitle: NSLocalizedString("Open rushbin", comment: ""), action: #selector(show), keyEquivalent: "").target = self
        menu.addItem(.separator())
        menu.addItem(withTitle: NSLocalizedString("Quit rushbin", comment: ""), action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
    }

    // Ejecting can take seconds (the drive flushes): off the main thread. macOS says why when it cannot.
    @objc func eject(_ item: NSMenuItem) {
        guard let mount = item.representedObject as? String else { return }
        DispatchQueue.global(qos: .userInitiated).async {
            do { try NSWorkspace.shared.unmountAndEjectDevice(at: URL(fileURLWithPath: mount)) } catch {
                DispatchQueue.main.async { self.fail(error.localizedDescription) }
            }
        }
    }

    // The menu's commands for the page (rushbinMenu): the views, search, and the clip keys it already has.
    @objc func menuCommand(_ item: NSMenuItem) {
        guard let cmd = item.representedObject as? String else { return }
        show()
        web.evaluateJavaScript("window.rushbinMenu && rushbinMenu('\(cmd)')")
    }

    func validateMenuItem(_ item: NSMenuItem) -> Bool {
        guard item.action == #selector(menuCommand(_:)), let cmd = item.representedObject as? String else { return true }
        return pageReady && (!cmd.hasPrefix("clip:") || clipSelected)
    }

    // Text size, kept for the next launch.
    @objc func zoomIn(_ sender: Any?) { setZoom(web.pageZoom + 0.1) }
    @objc func zoomOut(_ sender: Any?) { setZoom(web.pageZoom - 0.1) }
    @objc func zoomReset(_ sender: Any?) { setZoom(1) }
    func setZoom(_ z: CGFloat) {
        web.pageZoom = min(max(z, 0.6), 2)
        UserDefaults.standard.set(web.pageZoom, forKey: "zoom")
    }

    @objc func openLink(_ item: NSMenuItem) {
        if let u = item.representedObject as? URL { NSWorkspace.shared.open(u) }
    }

    // Only the engine's own pages open inside the window.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if let u = action.request.url, u.host == url?.host, u.port == url?.port { return decisionHandler(.allow) }
        if let u = action.request.url, u.scheme == "https" { NSWorkspace.shared.open(u) }
        decisionHandler(.cancel)
    }

    // The menu bar, in the order Apple's apps use: the app, Edit, Go (the views), Clip (the keys of the page),
    // View, Window, Help. No File menu: rushbin opens and saves no documents.
    func buildMenu() {
        UserDefaults.standard.set(false, forKey: "NSFullScreenMenuItemEverywhere") // View has its own
        let main = NSMenu()
        func menu(_ title: String, _ items: [NSMenuItem]) -> NSMenu {
            let m = NSMenu(title: title)
            items.forEach(m.addItem)
            let holder = NSMenuItem()
            holder.submenu = m
            main.addItem(holder)
            return m
        }
        func item(_ title: String, _ action: Selector?, _ key: String = "", _ mods: NSEvent.ModifierFlags = .command, target: AnyObject? = nil, object: Any? = nil) -> NSMenuItem {
            let i = NSMenuItem(title: NSLocalizedString(title, comment: ""), action: action, keyEquivalent: key)
            i.keyEquivalentModifierMask = mods
            i.target = target
            i.representedObject = object
            return i
        }
        func command(_ title: String, _ cmd: String, _ key: String = "", _ mods: NSEvent.ModifierFlags = .command) -> NSMenuItem {
            item(title, #selector(menuCommand(_:)), key, mods, target: self, object: cmd)
        }
        let services = item("Services", nil)
        services.submenu = NSMenu()
        NSApp.servicesMenu = services.submenu
        _ = menu("rushbin", [ // macOS shows the app's name
            item("About rushbin", #selector(NSApplication.orderFrontStandardAboutPanel(_:))),
            item("Check for Updates…", #selector(SPUStandardUpdaterController.checkForUpdates(_:)), target: updates),
            .separator(), services, .separator(),
            item("Hide rushbin", #selector(NSApplication.hide(_:)), "h"),
            item("Hide Others", #selector(NSApplication.hideOtherApplications(_:)), "h", [.command, .option]),
            item("Show All", #selector(NSApplication.unhideAllApplications(_:))),
            .separator(),
            item("Quit rushbin", #selector(NSApplication.terminate(_:)), "q"),
        ])
        _ = menu(NSLocalizedString("Edit", comment: ""), [ // the search field and the forms
            item("Undo", Selector(("undo:")), "z"), item("Redo", Selector(("redo:")), "z", [.command, .shift]), .separator(),
            item("Cut", #selector(NSText.cut(_:)), "x"), item("Copy", #selector(NSText.copy(_:)), "c"),
            item("Paste", #selector(NSText.paste(_:)), "v"), item("Select All", #selector(NSText.selectAll(_:)), "a"),
        ])
        _ = menu(NSLocalizedString("Go", comment: ""), [
            command("Archive", "go:archive", "1"), command("Duplicates", "go:dups", "2"),
            command("Drives", "go:drives", "3"), command("Team", "go:team", "4"), .separator(),
            command("Search", "search", "f"),
        ])
        let none: NSEvent.ModifierFlags = []
        _ = menu(NSLocalizedString("Clip", comment: ""), [ // the keys the page already answers, here so they can be found
            command("Open or Play", "clip:play", "\r", none), .separator(),
            command("Previous", "clip:prev", String(UnicodeScalar(NSLeftArrowFunctionKey)!), none),
            command("Next", "clip:next", String(UnicodeScalar(NSRightArrowFunctionKey)!), none), .separator(),
            command("Keep", "clip:keep", "k", none), command("Showreel", "clip:showreel", "s", none),
            command("Discard", "clip:discard", "x", none), .separator(),
            command("Close Clip", "clip:close", "\u{1b}", none),
        ])
        _ = menu(NSLocalizedString("View", comment: ""), [
            item("Bigger Text", #selector(zoomIn(_:)), "+", target: self), item("Smaller Text", #selector(zoomOut(_:)), "-", target: self),
            item("Actual Size", #selector(zoomReset(_:)), "0", target: self), .separator(),
            item("Enter Full Screen", #selector(NSWindow.toggleFullScreen(_:)), "f", [.command, .control]),
        ])
        let win = menu(NSLocalizedString("Window", comment: ""), [
            item("Close", #selector(NSWindow.performClose(_:)), "w"), item("Minimize", #selector(NSWindow.performMiniaturize(_:)), "m"),
            item("Zoom", #selector(NSWindow.performZoom(_:))), .separator(),
            item("Bring All to Front", #selector(NSApplication.arrangeInFront(_:))),
        ])
        let help = menu(NSLocalizedString("Help", comment: ""), [
            item("rushbin Guides", #selector(openLink(_:)), target: self, object: URL(string: "https://rushbin.io/guides")!),
            item("Report a Problem", #selector(openLink(_:)), target: self, object: URL(string: "https://github.com/artideinnova/rushbin/issues")!),
        ])
        NSApp.mainMenu = main
        NSApp.windowsMenu = win
        NSApp.helpMenu = help
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
