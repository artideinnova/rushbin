// rushbin for Mac: a window and a menu bar item around the engine (rushbin-engine), which does the work
// and serves the interface on 127.0.0.1 to this window only. Sparkle keeps it up to date. Its own few words
// are in macos/<language>.lproj (English, the keys, when none fits); the page has its own (web/i18n).
import AppKit
import IOKit.ps
import Network
import Sparkle
import WebKit

final class AppDelegate: NSObject, NSApplicationDelegate, WKUIDelegate, WKNavigationDelegate, SPUUpdaterDelegate {
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

    // Sparkle asks the engine's update site (its team's, or Artide's only if allowed), checks in the background
    // and downloads by itself (Info.plist), and installs when rushbin quits.
    func feedURLString(for updater: SPUUpdater) -> String? { feed }

    func updater(_ updater: SPUUpdater, mayPerform updateCheck: SPUUpdateCheck) throws {
        if feed == nil { throw NSError(domain: "rushbin", code: 1, userInfo: [NSLocalizedDescriptionKey: "No update site: not allowed to ask Artide's site, and in no team."]) }
    }

    func applicationDidFinishLaunching(_ note: Notification) {
        buildMenu()
        updates = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: self, userDriverDelegate: nil)
        web = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
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
        status.button?.image = NSImage(systemSymbolName: "film.stack", accessibilityDescription: "rushbin")
        let menu = NSMenu()
        menu.addItem(withTitle: NSLocalizedString("Open rushbin", comment: ""), action: #selector(show), keyEquivalent: "")
        menu.addItem(.separator())
        menu.addItem(withTitle: NSLocalizedString("Quit rushbin", comment: ""), action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        status.menu = menu

        startEngine()
    }

    func startEngine() {
        guard let exe = Bundle.main.url(forAuxiliaryExecutable: "rushbin-engine") else { return fail(NSLocalizedString("The engine is missing from the app.", comment: "")) }
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
            DispatchQueue.main.async { if NSApp.isRunning, proc.terminationReason != .exit || proc.terminationStatus != 0 { self?.fail(String(format: NSLocalizedString("The engine stopped (code %d). Quit and reopen rushbin.", comment: ""), proc.terminationStatus)) } }
        }
        do { try p.run() } catch { return fail(String(format: NSLocalizedString("The engine could not start: %@", comment: ""), error.localizedDescription)) }
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
            DispatchQueue.main.async { if self.url == nil { self.fail(NSLocalizedString("The engine did not answer.", comment: "")) } }
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

    // Only the engine's own pages open inside the window.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if let u = action.request.url, u.host == url?.host, u.port == url?.port { return decisionHandler(.allow) }
        if let u = action.request.url, u.scheme == "https" { NSWorkspace.shared.open(u) }
        decisionHandler(.cancel)
    }

    func buildMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: NSLocalizedString("About rushbin", comment: ""), action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: NSLocalizedString("Hide rushbin", comment: ""), action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: NSLocalizedString("Quit rushbin", comment: ""), action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        main.addItem(appItem)
        let editItem = NSMenuItem() // copy and paste in the search field
        let edit = NSMenu(title: NSLocalizedString("Edit", comment: ""))
        edit.addItem(withTitle: NSLocalizedString("Cut", comment: ""), action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: NSLocalizedString("Copy", comment: ""), action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: NSLocalizedString("Paste", comment: ""), action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: NSLocalizedString("Select All", comment: ""), action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        main.addItem(editItem)
        let windowItem = NSMenuItem()
        let win = NSMenu(title: NSLocalizedString("Window", comment: ""))
        win.addItem(withTitle: NSLocalizedString("Close", comment: ""), action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        win.addItem(withTitle: NSLocalizedString("Minimize", comment: ""), action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        windowItem.submenu = win
        main.addItem(windowItem)
        NSApp.mainMenu = main
        NSApp.windowsMenu = win
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
