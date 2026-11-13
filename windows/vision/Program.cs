// rushbin-vision for Windows: the same input and output as macos/vision.swift, so vision.go drives both.
//   rushbin-vision [MODEL.onnx] < folders
// One preview folder per line (1.jpg … 5.jpg), with what to do after a tab: "look", "embed" or "both".
// look: one JSON line per folder out. Per frame: contrast (standard deviation of the brightness on a 256 px
// wide gray copy, 0 for a blank frame) and text (lines read on screen by Windows OCR, in the languages of the
// user's profile). No labels: Windows has no classifier like Apple's; SigLIP covers what they were for.
// embed: the image half of SigLIP 2 (ONNX, downloaded by rushbin: vision.go) on DirectML, the GPU of any
// vendor, or on the CPU without one; writes emb.bin in the folder like the Mac: one 768-number embedding per
// frame, float16, little endian, for search by content (search.go).
using System.Runtime.InteropServices.WindowsRuntime;
using System.Text;
using System.Text.Json;
using Microsoft.ML.OnnxRuntime;
using Microsoft.ML.OnnxRuntime.Tensors;
using Windows.Graphics.Imaging;
using Windows.Media.Ocr;
using Windows.Storage;

InferenceSession? siglip = null;
if (args.Length > 0)
{
    // DirectML runs one inference at a time, without memory patterns (onnxruntime's documentation).
    var o = new SessionOptions { EnableMemoryPattern = false, ExecutionMode = ExecutionMode.ORT_SEQUENTIAL };
    try
    {
        o.AppendExecutionProvider_DML(0); // the default adapter; onnxruntime refuses the software one
        Console.Error.WriteLine("rushbin-vision: SigLIP 2 on DirectML");
    }
    catch (OnnxRuntimeException e)
    {
        Console.Error.WriteLine($"rushbin-vision: SigLIP 2 on the CPU (no DirectML: {e.Message})");
    }
    siglip = new InferenceSession(args[0], o);
}
Console.Error.WriteLine($"rushbin-vision: OCR in {OcrEngine.TryCreateFromUserProfileLanguages()?.RecognizerLanguage.LanguageTag ?? "no language"}");

var dirs = new List<(string Dir, string Mode)>();
using (var input = new StreamReader(Console.OpenStandardInput(), new UTF8Encoding(false)))
{
    for (string? line; (line = input.ReadLine()) != null;)
    {
        var p = line.Split('\t', 2);
        if (p[0] != "") dirs.Add((p[0], p.Length > 1 ? p[1] : "look"));
    }
}

var stdout = Console.OpenStandardOutput();
// 4 folders at a time, like the Mac.
await Parallel.ForEachAsync(dirs, new ParallelOptions { MaxDegreeOfParallelism = 4 }, async (d, _) =>
{
    var frames = new List<SoftwareBitmap?>();
    for (int i = 1; i <= 5; i++)
    {
        var p = Path.Combine(d.Dir, $"{i}.jpg");
        if (File.Exists(p)) frames.Add(await Load(p));
    }
    if (d.Mode != "look" && siglip != null) Embed(siglip, d.Dir, frames);
    if (d.Mode == "embed") return;
    var ocr = OcrEngine.TryCreateFromUserProfileLanguages();
    using var buf = new MemoryStream();
    using (var w = new Utf8JsonWriter(buf))
    {
        w.WriteStartObject();
        w.WriteString("dir", d.Dir);
        w.WriteStartArray("frames");
        foreach (var f in frames)
        {
            w.WriteStartObject();
            if (f == null)
            {
                w.WriteString("error", "load");
            }
            else
            {
                w.WriteNumber("contrast", Contrast(f));
                w.WriteStartArray("text");
                if (ocr != null)
                {
                    foreach (var l in (await ocr.RecognizeAsync(f)).Lines)
                    {
                        if (l.Text.Length >= 3) w.WriteStringValue(l.Text);
                    }
                }
                w.WriteEndArray();
            }
            w.WriteEndObject();
        }
        w.WriteEndArray();
        w.WriteEndObject();
    }
    buf.WriteByte((byte)'\n');
    lock (stdout)
    {
        buf.WriteTo(stdout);
        stdout.Flush();
    }
});

static async Task<SoftwareBitmap?> Load(string path)
{
    try
    {
        var file = await StorageFile.GetFileFromPathAsync(Path.GetFullPath(path));
        using var s = await file.OpenReadAsync();
        var dec = await BitmapDecoder.CreateAsync(s);
        return await dec.GetSoftwareBitmapAsync(BitmapPixelFormat.Bgra8, BitmapAlphaMode.Ignore);
    }
    catch (Exception)
    {
        return null;
    }
}

// Embed writes emb.bin: the frames squashed to 256×256 (no crop, like the SigLIP processor), in [-1, 1],
// through the model in one batch, each embedding normalized (the ONNX export does not normalize).
static void Embed(InferenceSession siglip, string dir, List<SoftwareBitmap?> frames)
{
    var ok = frames.OfType<SoftwareBitmap>().ToList();
    if (ok.Count == 0) return;
    const int n = 256, plane = n * n;
    var x = new DenseTensor<float>([ok.Count, 3, n, n]);
    var data = x.Buffer.Span;
    for (int i = 0; i < ok.Count; i++)
    {
        var rgb = Resize(ok[i], n, n);
        for (int j = 0; j < 3 * plane; j++) data[i * 3 * plane + j] = rgb[j] / 127.5f - 1;
    }
    float[] v;
    lock (siglip) // one inference at a time on DirectML
    {
        using var r = siglip.Run([NamedOnnxValue.CreateFromTensor("pixel_values", x)], ["pooler_output"]);
        v = r[0].AsTensor<float>().ToArray();
    }
    var b = new byte[v.Length * 2];
    for (int i = 0; i < ok.Count; i++)
    {
        var e = v.AsSpan(i * 768, 768);
        float norm = 0;
        foreach (var f in e) norm += f * f;
        norm = MathF.Sqrt(norm);
        for (int j = 0; j < 768; j++) BitConverter.TryWriteBytes(b.AsSpan((i * 768 + j) * 2), (Half)(e[j] / norm));
    }
    var tmp = Path.Combine(dir, "emb.bin.part");
    File.WriteAllBytes(tmp, b);
    File.Move(tmp, Path.Combine(dir, "emb.bin"), true);
}

// Contrast: standard deviation of the brightness (Rec. 601 luma) on a 256 px wide copy, rounded like the Mac.
static double Contrast(SoftwareBitmap f)
{
    int w = 256, h = Math.Max(1, (int)(f.PixelHeight * 256.0 / f.PixelWidth));
    var rgb = Resize(f, w, h);
    int n = w * h;
    double sum = 0, sq = 0;
    for (int i = 0; i < n; i++)
    {
        double y = (0.299 * rgb[i] + 0.587 * rgb[n + i] + 0.114 * rgb[2 * n + i]) / 255;
        sum += y;
        sq += y * y;
    }
    double mean = sum / n;
    return Math.Round(Math.Sqrt(Math.Max(0, sq / n - mean * mean)), 3);
}

// Resize scales a frame to w×h, ignoring its proportions, into planes R, G, B (0…255): Pillow's antialiased
// resampling with the Hamming window, the one closest to the Mac's Core Graphics (vision_test.go, make.sh).
static float[] Resize(SoftwareBitmap f, int w, int h)
{
    int sw = f.PixelWidth, sh = f.PixelHeight;
    var px = new byte[sw * sh * 4]; // BGRA
    f.CopyToBuffer(px.AsBuffer());
    var (xs, xw) = Weights(sw, w);
    var (ys, yw) = Weights(sh, h);
    var tmp = new float[sh * w * 3]; // across first, rows of RGB
    for (int y = 0; y < sh; y++)
    {
        for (int x = 0; x < w; x++)
        {
            float r = 0, g = 0, b = 0;
            var k = xw[x];
            for (int i = 0; i < k.Length; i++)
            {
                int p = (y * sw + xs[x] + i) * 4;
                b += px[p] * k[i];
                g += px[p + 1] * k[i];
                r += px[p + 2] * k[i];
            }
            int o = (y * w + x) * 3;
            tmp[o] = r;
            tmp[o + 1] = g;
            tmp[o + 2] = b;
        }
    }
    var res = new float[3 * w * h]; // then down, into planes
    for (int y = 0; y < h; y++)
    {
        var k = yw[y];
        for (int x = 0; x < w; x++)
        {
            float r = 0, g = 0, b = 0;
            for (int i = 0; i < k.Length; i++)
            {
                int p = ((ys[y] + i) * w + x) * 3;
                r += tmp[p] * k[i];
                g += tmp[p + 1] * k[i];
                b += tmp[p + 2] * k[i];
            }
            res[y * w + x] = r;
            res[w * h + y * w + x] = g;
            res[2 * w * h + y * w + x] = b;
        }
    }
    return res;
}

// Weights: for each output pixel, the first input pixel and the normalized filter weights (Pillow's
// precompute_coeffs: the filter widens with the scale, so shrinking averages instead of skipping pixels).
static (int[], float[][]) Weights(int inSize, int outSize)
{
    double scale = (double)inSize / outSize, fs = Math.Max(scale, 1);
    var start = new int[outSize];
    var w = new float[outSize][];
    for (int i = 0; i < outSize; i++)
    {
        double c = (i + 0.5) * scale;
        int lo = Math.Max((int)(c - fs + 0.5), 0), hi = Math.Min((int)(c + fs + 0.5), inSize);
        var k = new double[hi - lo];
        for (int j = 0; j < k.Length; j++) k[j] = Hamming((j + lo - c + 0.5) / fs);
        double sum = k.Sum();
        start[i] = lo;
        w[i] = k.Select(v => (float)(v / sum)).ToArray();
    }
    return (start, w);
}

static double Hamming(double x)
{
    x = Math.Abs(x);
    if (x == 0) return 1;
    if (x >= 1) return 0;
    x *= Math.PI;
    return Math.Sin(x) / x * (0.54 + 0.46 * Math.Cos(x));
}
