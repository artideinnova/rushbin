# Notices

rushbin is © 2026 Artide Innova SRL, under the [Functional Source License 1.1, Apache 2.0 future license](LICENSE.md).
It includes or uses software and models by others, each under its own license. The official apps carry these licenses'
texts, this file and `GO-LICENSES.txt` (the Go standard library and modules inside them): on the Mac in
rushbin.app/Contents/Resources, on Windows in the app's folder.

## In the source code and the site

| Component | License | Use |
|---|---|---|
| [pgx](https://github.com/jackc/pgx), pgpassfile, pgservicefile, puddle | MIT | Postgres driver of the team site |
| [Go](https://go.dev) standard library, golang.org/x/crypto, sync, sys, text | BSD 3-Clause | The engine and the site |
| [Caddy](https://caddyserver.com) | Apache 2.0 | HTTPS of the team site (a container image, not modified) |
| [PostgreSQL](https://www.postgresql.org) with [pgvector](https://github.com/pgvector/pgvector) | PostgreSQL License | The team site's database (container images) |
| Google's [SigLIP 2](https://huggingface.co/google/siglip2-base-patch16-256), run with PyTorch and Hugging Face Transformers | Apache 2.0 (model, Transformers), BSD 3-Clause (PyTorch) | The team site's search by looks (`deploy/text`, fetched when the image is built) |
| Sintel and Big Buck Bunny, © Blender Foundation | CC BY 3.0 | Frames in the screenshot of the README |

## In the official apps

| Component | License | Use |
|---|---|---|
| [FFmpeg](https://ffmpeg.org) | LGPL 2.1 or later | Reading and converting video. Built LGPL-only from the official sources with `macos/build-ffmpeg.sh`, as separate programs the app runs |
| [whisper.cpp](https://github.com/ggml-org/whisper.cpp) | MIT | Writing down what is said (`macos/build-whisper.sh`) |
| [Sparkle](https://sparkle-project.org) | MIT | The Mac app's updates |
| The image half of Google's [SigLIP 2](https://huggingface.co/google/siglip2-base-patch16-256) | Apache 2.0 | Search by looks, converted for the Mac (`macos/siglip`) |
| RED's R3D SDK libraries (REDR3D, REDMetal) | RED's own license | Reading RED and Nikon raw. Official Mac builds only: never in this repository |
| [ONNX Runtime](https://onnxruntime.ai) | MIT | Windows: search by looks |
| DirectML | Microsoft's license (in the app) | Windows: search by looks on any DirectX 12 GPU |
| Microsoft Visual C++ runtime | Microsoft's redistributable license | Windows: needed by ONNX Runtime and whisper.cpp |

## Downloaded by the apps on first use

| Model | License | From |
|---|---|---|
| Whisper large-v3-turbo (OpenAI), in ggml form | MIT | Hugging Face |
| Silero VAD, in ggml form | MIT | Hugging Face |
| The text half of SigLIP 2, converted for the Mac (Mac app in no team) | Apache 2.0 | Hugging Face, artideinnova |
| SigLIP 2 in ONNX form (onnx-community), for Windows | Apache 2.0 | Hugging Face |
