// rushbin-vision: looks at the preview frames of each clip on this Mac.
//   rushbin-vision [MODEL.mlpackage CACHE.mlmodelc] < folders
// One preview folder per line (1.jpg … 5.jpg), with what to do after a tab: "look", "embed" or "both".
// look: Apple Vision, one JSON line per folder out. Per frame: contrast (standard deviation of the brightness,
// 0 for a blank frame), labels (Apple classification, kept where it reaches 90% precision) and text (lines read
// on screen). The engine turns them into suggestions and filters (vision.go).
// embed: with the SigLIP 2 image model (Core ML, converted by macos/siglip/convert.py), writes emb.bin in the
// folder: one 768-number embedding per frame, float16, little endian, for search by content (search.go).
import CoreML
import Foundation
import Vision
import CoreGraphics
import ImageIO
import Accelerate
import AVFoundation
import SoundAnalysis

// rushbin-vision frame FILE SECONDS | frames FILE START: video FFmpeg cannot decode in hardware (ProRes RAW,
// whose Bayer output VideoToolbox will not hand to FFmpeg: -12905), decoded by AVFoundation on the Mac's media
// engine, debayered with the clip's own color, as PPM on stdout like rushbin-red: one frame, or every frame
// from START at most 1280 px wide for light copies and playback.
if CommandLine.arguments.count == 4, ["frame", "frames"].contains(CommandLine.arguments[1]) {
    let asset = AVURLAsset(url: URL(fileURLWithPath: CommandLine.arguments[2]))
    let at = CMTime(seconds: Double(CommandLine.arguments[3]) ?? 0, preferredTimescale: 600)
    let stdout = FileHandle.standardOutput
    func ppm(_ base: UnsafeRawPointer, _ w: Int, _ h: Int, _ row: Int) { // BGRA in, RGB out
        var d = Data("P6\n\(w) \(h)\n255\n".utf8)
        d.reserveCapacity(d.count + w * h * 3)
        for y in 0..<h {
            let p = base.advanced(by: y * row).assumingMemoryBound(to: UInt8.self)
            for x in 0..<w { d.append(p[x * 4 + 2]); d.append(p[x * 4 + 1]); d.append(p[x * 4]) }
        }
        stdout.write(d)
    }
    if CommandLine.arguments[1] == "frame" {
        let gen = AVAssetImageGenerator(asset: asset)
        gen.appliesPreferredTrackTransform = true
        gen.maximumSize = CGSize(width: 1280, height: 1280)
        gen.requestedTimeToleranceBefore = .zero
        gen.requestedTimeToleranceAfter = .zero
        guard let cg = try? gen.copyCGImage(at: at, actualTime: nil) else { exit(1) }
        let w = cg.width, h = cg.height
        var px = [UInt8](repeating: 0, count: w * h * 4)
        px.withUnsafeMutableBytes { buf in
            let ctx = CGContext(data: buf.baseAddress, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4, space: CGColorSpaceCreateDeviceRGB(),
                                bitmapInfo: CGImageAlphaInfo.premultipliedFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue)
            ctx?.draw(cg, in: CGRect(x: 0, y: 0, width: w, height: h))
            ppm(UnsafeRawPointer(buf.baseAddress!), w, h, w * 4)
        }
        exit(0)
    }
    guard let track = asset.tracks(withMediaType: .video).first, let reader = try? AVAssetReader(asset: asset) else { exit(1) }
    let size = track.naturalSize, w = min(1280, Int(size.width)) & ~1
    let h = (Int(size.height * CGFloat(w) / size.width)) & ~1
    let output = AVAssetReaderTrackOutput(track: track, outputSettings: [kCVPixelBufferPixelFormatTypeKey as String: kCVPixelFormatType_32BGRA,
        kCVPixelBufferWidthKey as String: w, kCVPixelBufferHeightKey as String: h]) // scaled by the decoder
    reader.timeRange = CMTimeRange(start: at, end: .positiveInfinity)
    reader.add(output)
    guard reader.startReading() else { exit(1) }
    while let sample = output.copyNextSampleBuffer() {
        guard let pb = CMSampleBufferGetImageBuffer(sample) else { continue }
        CVPixelBufferLockBaseAddress(pb, .readOnly)
        ppm(CVPixelBufferGetBaseAddress(pb)!, CVPixelBufferGetWidth(pb), CVPixelBufferGetHeight(pb), CVPixelBufferGetBytesPerRow(pb))
        CVPixelBufferUnlockBaseAddress(pb, .readOnly)
    }
    exit(reader.status == .completed ? 0 : 1)
}

// rushbin-vision text MODEL.mlmodelc: the text half of SigLIP 2 (macos/siglip/convert-text.py), for search by
// looks on a Mac in no team. Kept running between searches: one line of 64 token ids in (text.go tokenizes),
// one line out, the normalized embedding as a JSON array of 768 numbers ([] when it fails).
// On the Neural Engine: 1.4 ms a search on an M5 Pro against 2.1 ms on the GPU (12 November 2026).
if CommandLine.arguments.count == 3, CommandLine.arguments[1] == "text" {
    let cfg = MLModelConfiguration()
    cfg.computeUnits = .cpuAndNeuralEngine
    guard let model = try? MLModel(contentsOf: URL(fileURLWithPath: CommandLine.arguments[2]), configuration: cfg),
          let ids = try? MLMultiArray(shape: [1, 64], dataType: .int32) else { exit(1) }
    let p = ids.dataPointer.assumingMemoryBound(to: Int32.self)
    while let line = readLine() {
        let v = line.split(separator: " ").compactMap { Int32($0) }
        var out = "[]"
        if v.count == 64 {
            for (i, id) in v.enumerated() { p[i] = id }
            if let o = try? model.prediction(from: MLDictionaryFeatureProvider(dictionary: ["ids": ids])),
               let e = o.featureValue(for: "embedding")?.multiArrayValue {
                out = "[" + (0..<e.count).map { "\(e[$0].floatValue)" }.joined(separator: ",") + "]"
            }
        }
        FileHandle.standardOutput.write(Data((out + "\n").utf8))
    }
    exit(0)
}

// rushbin-vision voice WAV: how much of a human voice, speaking or singing, Apple's sound classifier (on device)
// hears in each segment Whisper wrote (speech.go). One line in per segment, "START END" in seconds; one line
// out, the score: the highest confidence of the voice labels over the 3 s windows (half overlapping) that touch
// the segment with 0.5 s more on each side. The whole file is classified once (a file shorter than one window is
// completed with silence): one analysis per segment cost 0.11 s a segment, 32 min for a 1801-segment show.
if CommandLine.arguments.count == 3, CommandLine.arguments[1] == "voice" {
    final class Voice: NSObject, SNResultsObserving {
        static let labels = ["speech", "shout", "yell", "children_shouting", "whispering", "singing", "choir_singing", "rapping", "humming"]
        var windows: [(start: Double, end: Double, score: Double)] = []
        func request(_ request: SNRequest, didProduce result: SNResult) {
            guard let r = result as? SNClassificationResult else { return }
            let score = Voice.labels.map { r.classification(forIdentifier: $0)?.confidence ?? 0 }.max()!
            windows.append((r.timeRange.start.seconds, r.timeRange.end.seconds, score))
        }
    }
    guard let file = try? AVAudioFile(forReading: URL(fileURLWithPath: CommandLine.arguments[2])),
          case let format = file.processingFormat, format.channelCount == 1,
          let all = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(max(file.length, Int64(3 * format.sampleRate)))),
          (try? file.read(into: all)) != nil else { exit(1) }
    (all.floatChannelData![0] + Int(all.frameLength)).update(repeating: 0, count: Int(all.frameCapacity - all.frameLength))
    all.frameLength = all.frameCapacity
    let req = try! SNClassifySoundRequest(classifierIdentifier: .version1)
    req.windowDuration = CMTime(seconds: 3, preferredTimescale: 48000)
    req.overlapFactor = 0.5
    let analyzer = SNAudioStreamAnalyzer(format: format), v = Voice()
    try! analyzer.add(req, withObserver: v)
    analyzer.analyze(all, atAudioFramePosition: 0)
    analyzer.completeAnalysis() // returns once every window has been reported
    while let line = readLine() {
        let t = line.split(separator: " ").compactMap { Double($0) }
        guard t.count == 2 else { exit(1) }
        let score = v.windows.filter { $0.start < t[1] + 0.5 && $0.end > t[0] - 0.5 }.map(\.score).max() ?? 0
        print(String(format: "%.4f", score))
    }
    exit(0)
}

struct Frame: Encodable {
    var contrast: Float?
    var labels: [String: Float] = [:]
    var text: [String] = []
    var error: String?
}

struct Out: Encodable {
    let dir: String
    var frames: [Frame] = []
}

// contrast: standard deviation of the brightness on a 256 px wide gray copy.
func contrast(_ cg: CGImage) -> Float? {
    let w = 256, h = max(1, Int(Double(cg.height) * 256.0 / Double(cg.width)))
    var px = [UInt8](repeating: 0, count: w * h)
    let ok = px.withUnsafeMutableBytes { buf -> Bool in
        guard let ctx = CGContext(data: buf.baseAddress, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w,
                                  space: CGColorSpaceCreateDeviceGray(), bitmapInfo: CGImageAlphaInfo.none.rawValue) else { return false }
        ctx.draw(cg, in: CGRect(x: 0, y: 0, width: w, height: h))
        return true
    }
    guard ok else { return nil }
    var mean: Float = 0, sd: Float = 0
    vDSP_normalize(px.map { Float($0) / 255 }, 1, nil, 1, &mean, &sd, vDSP_Length(px.count))
    return (sd * 1000).rounded() / 1000
}

func round2(_ v: Float) -> Float { (v * 100).rounded() / 100 }

func analyze(_ path: String) -> Frame {
    var o = Frame()
    guard let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: path) as CFURL, nil),
          let cg = CGImageSourceCreateImageAtIndex(src, 0, nil) else { o.error = "load"; return o }
    let cls = VNClassifyImageRequest()
    let txt = VNRecognizeTextRequest()
    txt.recognitionLevel = .accurate
    txt.automaticallyDetectsLanguage = true
    let reqs: [VNRequest] = [cls, txt]
    do {
        try VNImageRequestHandler(cgImage: cg).perform(reqs)
        // Apple's recommended filter for search: labels that reach 90% precision. Without the classes that put
        // people into categories by age or body feature (personLabels in vision.go, which drops them also from
        // what older versions stored).
        let personLabels: Set = ["adult", "baby", "child", "teen", "tattoo"]
        for c in cls.results ?? [] where c.hasMinimumRecall(0.01, forPrecision: 0.9) && !personLabels.contains(c.identifier) {
            o.labels[c.identifier] = round2(c.confidence)
        }
        for t in txt.results ?? [] {
            if let s = t.topCandidates(1).first, s.confidence >= 0.5, s.string.count >= 3 { o.text.append(s.string) }
        }
    } catch { o.error = "\(error)" }
    o.contrast = contrast(cg)
    return o
}

// half converts to float16 with vImage (Swift's Float16 does not exist on Intel Macs), little endian.
func half(_ v: [Float]) -> Data? {
    var src = v, out = [UInt16](repeating: 0, count: v.count)
    let n = vImagePixelCount(v.count)
    let err = src.withUnsafeMutableBytes { s in
        out.withUnsafeMutableBytes { d in
            var a = vImage_Buffer(data: s.baseAddress, height: 1, width: n, rowBytes: v.count * 4)
            var b = vImage_Buffer(data: d.baseAddress, height: 1, width: n, rowBytes: v.count * 2)
            return vImageConvert_PlanarFtoPlanar16F(&a, &b, vImage_Flags(kvImageNoFlags))
        }
    }
    return err == kvImageNoError ? out.withUnsafeBufferPointer { Data(buffer: $0) } : nil
}

// squash scales the image to n×n, ignoring its proportions, like the SigLIP processor.
let quality: CGInterpolationQuality = ["none": .none, "low": .low, "medium": .medium, "high": .high][ProcessInfo.processInfo.environment["RV_INTERP"] ?? ""] ?? .medium
func squash(_ cg: CGImage, _ n: Int) -> CGImage? {
    guard let ctx = CGContext(data: nil, width: n, height: n, bitsPerComponent: 8, bytesPerRow: 0,
                              space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { return nil }
    ctx.interpolationQuality = quality
    ctx.draw(cg, in: CGRect(x: 0, y: 0, width: n, height: n))
    return ctx.makeImage()
}

// embedder runs SigLIP 2 on a frame; one prediction at a time (MLModel is not documented as thread-safe).
final class Embedder: @unchecked Sendable {
    let model: MLModel
    let constraint: MLImageConstraint
    let lock = NSLock()
    // The package is compiled for this Mac the first time, into cache.
    init(_ pkg: URL, cache: URL) throws {
        if !FileManager.default.fileExists(atPath: cache.path) {
            let tmp = try MLModel.compileModel(at: pkg)
            try? FileManager.default.createDirectory(at: cache.deletingLastPathComponent(), withIntermediateDirectories: true)
            try FileManager.default.moveItem(at: tmp, to: cache)
        }
        let url = cache
        let cfg = MLModelConfiguration()
        cfg.computeUnits = .all
        model = try MLModel(contentsOf: url, configuration: cfg)
        constraint = model.modelDescription.inputDescriptionsByName["image"]!.imageConstraint!
    }
    // The image squashed to 256×256, like the SigLIP processor does (no crop).
    func embed(_ path: String) -> [Float]? {
        guard let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: path) as CFURL, nil),
              let cg = CGImageSourceCreateImageAtIndex(src, 0, nil),
              let small = squash(cg, 256),
              let img = try? MLFeatureValue(cgImage: small, constraint: constraint, options: nil) else { return nil }
        lock.lock(); defer { lock.unlock() }
        guard let out = try? model.prediction(from: MLDictionaryFeatureProvider(dictionary: ["image": img])),
              let a = out.featureValue(for: "embedding")?.multiArrayValue else { return nil }
        return (0..<a.count).map { a[$0].floatValue }
    }
}

let embedder: Embedder? = CommandLine.arguments.count > 2
    ? try? Embedder(URL(fileURLWithPath: CommandLine.arguments[1]), cache: URL(fileURLWithPath: CommandLine.arguments[2])) : nil

var dirs = [(String, String)]()
while let line = readLine() {
    let p = line.split(separator: "\t", maxSplits: 1).map(String.init)
    if let d = p.first, !d.isEmpty { dirs.append((d, p.count > 1 ? p[1] : "look")) }
}

let enc = JSONEncoder()
enc.outputFormatting = .sortedKeys
let lock = NSLock()
let out = FileHandle.standardOutput
// 4 folders at a time. With one task per core (DispatchQueue.concurrentPerform, 18 on the M-series Mac used
// to build it) Vision hung at the start of a run of 451 folders without using the CPU.
let q = OperationQueue()
q.maxConcurrentOperationCount = 4
for (dir, mode) in dirs {
    q.addOperation {
        let frames = (1...5).map { "\(dir)/\($0).jpg" }.filter { FileManager.default.fileExists(atPath: $0) }
        if mode != "look", let e = embedder {
            var v = [Float]()
            for p in frames { if let x = autoreleasepool(invoking: { e.embed(p) }) { v += x } }
            if !v.isEmpty, let data = half(v) {
                try? data.write(to: URL(fileURLWithPath: "\(dir)/emb.bin"), options: .atomic)
            }
        }
        guard mode != "embed" else { return }
        var o = Out(dir: dir)
        for p in frames { o.frames.append(autoreleasepool { analyze(p) }) }
        if var d = try? enc.encode(o) {
            d.append(0x0A)
            lock.lock(); out.write(d); lock.unlock()
        }
    }
}
q.waitUntilAllOperationsAreFinished()
