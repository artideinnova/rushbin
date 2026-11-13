// Draws every icon from web/logo.svg, the one logo of rushbin:
//   swift macos/icon.swift web/logo.svg OUT       → OUT/AppIcon.iconset (then iconutil -c icns) and
//                                                   OUT/apple-touch-icon.png, icon-192.png, icon-512.png
import AppKit

let args = CommandLine.arguments
guard args.count == 3, let logo = NSImage(contentsOfFile: args[1]) else {
    print("usage: swift macos/icon.swift web/logo.svg OUT"); exit(2)
}
let out = args[2]
let iconset = out + "/AppIcon.iconset"
try? FileManager.default.createDirectory(atPath: iconset, withIntermediateDirectories: true)

// draw renders the logo at px pixels; inset is the empty margin on each side (macOS icons use 10%).
// Home-screen icons get no margin and a solid background: iOS rounds the corners itself.
func draw(_ px: Int, inset: CGFloat, background: NSColor?, to path: String) {
    let s = CGFloat(px)
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8, samplesPerPixel: 4,
                               hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    NSGraphicsContext.current?.imageInterpolation = .high
    if let bg = background {
        bg.setFill()
        NSRect(x: 0, y: 0, width: s, height: s).fill()
    }
    logo.draw(in: NSRect(x: s * inset, y: s * inset, width: s * (1 - 2 * inset), height: s * (1 - 2 * inset)))
    NSGraphicsContext.restoreGraphicsState()
    try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: path))
}

for (px, name) in [(16, "16x16"), (32, "16x16@2x"), (32, "32x32"), (64, "32x32@2x"), (128, "128x128"), (256, "128x128@2x"),
                   (256, "256x256"), (512, "256x256@2x"), (512, "512x512"), (1024, "512x512@2x")] {
    draw(px, inset: 0.1, background: nil, to: "\(iconset)/icon_\(name).png")
}
for (px, name) in [(180, "apple-touch-icon"), (192, "icon-192"), (512, "icon-512")] {
    draw(px, inset: 0, background: NSColor(red: 0x12 / 255.0, green: 0x12 / 255.0, blue: 0x14 / 255.0, alpha: 1), // never pure black
          to: "\(out)/\(name).png")
}
