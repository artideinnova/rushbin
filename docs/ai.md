# AI in rushbin

What in rushbin is made by AI models, where it runs, how well it works, how people stay in control of it, and what
the EU AI Act (Regulation 2024/1689, as amended by Regulation 2026/1744) asks of it. Updated 13 November 2026.

## The components

| What | Model and version | Where it runs | Output |
|---|---|---|---|
| Speech to text | Whisper large-v3-turbo (`ggml-large-v3-turbo.bin`, SHA-256 `1fc70f77…`) in whisper.cpp v1.9.5 (commit `d1be6fde`), with the Silero VAD v5.1.2 voice detector (`speech.go`, `macos/build-whisper.sh`, `.github/workflows/ci.yml`) | The user's computer: the Mac's GPU (Metal); on Windows any GPU with Vulkan, else the CPU | Sentences with their start and end, in any language |
| Voice filter | Apple SoundAnalysis, `SNClassifySoundRequest` version 1 (`macos/vision.swift`, `voice`) | The Mac (not on Windows) | A score per sentence: how much of a voice the Mac hears there; not stored |
| Labels and text on screen | Apple Vision `VNClassifyImageRequest` (labels kept where they reach 90% precision) and `VNRecognizeTextRequest` (accurate, lines with confidence 0.5 or more); on Windows, Windows OCR (`Windows.Media.Ocr`) in the languages of the user's profile, no labels (`macos/vision.swift`, `windows/vision/Program.cs`) | The user's computer, on the 5 preview frames of each clip | Labels such as "crowd" or "stage", and lines of text read on screen |
| Search by looks | SigLIP 2 base (`google/siglip2-base-patch16-256`, Apache-2.0). Image half: Core ML package bundled in the Mac app (`macos/siglip/convert.py`), ONNX on Windows pinned to commit `d1114256` (`vision.go`). Text half: on the team site `deploy/text/server.py` (transformers, the model by name, revision not pinned); on a Mac in no team a Core ML copy pinned to commit `90d41162` (`text.go`) | Images: the user's computer (Neural Engine or GPU on the Mac, DirectML or CPU on Windows). Search words: the team site's CPU, or the Mac alone | Vectors of 768 numbers; never shown, only used to rank clips |
| Tidy up with AI | A large language model chosen by whoever pays for it: Anthropic (default `claude-opus-5-5`), OpenAI (default `gpt-6-astra`), Google (default `gemini-4-carbon`), or any OpenAI-compatible server the user sets; the user can change the model (`tidy.go`, `providers`). On Artide's own site only Anthropic (`siteAI`) | The provider's servers. The description goes there from the app with the user's own key, or through the team site with the site's key | A JSON list of proposed moves, each with a one-line reason |

Not AI: the "likely discard" suggestion is two fixed rules (every preview frame almost one flat color, or shorter
than 2 seconds: `vision.go`); duplicates are found by hashes.

## Known limits

- **Whisper invents text.** Measured on 12 November 2026 (large-v3-turbo, on the shoot-a and T9 archives): on music
  without voices it wrote 17 sentences nobody said ("Thank you.", "*music*", "Hmm..."). The voice filter removes
  them: they score 0.010 to 0.117, real speech 0.492 to 0.981 (901 sentences), sung lines 0.553 to 0.848 (15), and
  the filter keeps 0.3 and above (`speech.go`, `minVoice`). It does **not** remove text invented over real voices
  (crowds and choirs at feasts score 0.666 to 0.874) nor **repetition loops**, where Whisper writes the same line
  again and again. On Windows there is no voice filter. Measured on 5 November 2026: 10 minutes of interview gave
  952 words, the interviewer's question included; 7 s of party noise gave nothing.
- **Vision labels** come from Apple's fixed list of about 1,300 classes and can be wrong; text on screen can be
  misread, especially small, stylized or moving text. Windows gives no labels.
- **Search by looks** ranks by similarity: the first results are the nearest, not proven matches.
- **Tidy up with AI** can propose moves that make no sense. Every move is checked against the drive (it must exist,
  stay on the drive, not collide) before it is shown (`validTidy`).

## People in control

- The clip sheet says what is automatic: "Automatic transcript (Whisper). May contain errors." over what is said,
  "Recognized automatically." over labels and text on screen; the "Said" badge in results says the same.
- **Hide this line:** whoever can decide (editors and owners in a team; the user, alone) hides a wrong sentence.
  Hidden lines are not shown to the others and search does not find them; "Show again" brings one back.
- **Checked:** the same people mark a transcript as checked; the sheet shows who and when.
- Both are decisions like votes, with who took them and when: in a team the site keeps them per team, in the
  `decision` table under row level security, and every Mac of the team gets them (decision kinds `hide` and `check`
  in `catalog.go`). One design for the app and the site, no new table.
- A suggested discard is only a suggestion: nothing moves until someone presses X and confirms the trash.
- Tidy up with AI: the user confirms what is sent and to whom, ticks each move, moves are renames on the same drive,
  and the whole tidy can be undone. Nothing is deleted.

## Machine-readable marking

Wherever rushbin sends a transcript or what Vision recognized, it says that an AI made it:

- `speech`: `{"segs": [...], "generator": "whisper-large-v3-turbo", "ai_generated": true}`
- `look`: `{"labels": [...], "text": [...], "generator": "rushbin-vision", "ai_generated": true}`.
  `rushbin-vision` is Apple Vision on a Mac, Windows OCR on Windows.
- With them, in the clip (`/api/clip`): `hidden` (the starts, in seconds, of the hidden lines), `checked`,
  `checked_by`, `checked_at`.

The team export should carry the same field names on its transcripts and looks.

## The EU AI Act

Artide is the **provider** of these AI systems: it builds them into rushbin and distributes rushbin under its name
(art. 3(3)). A team that uses rushbin for its work is a **deployer** (art. 3(4)); so is Artide for its own archive.
rushbin provides no general-purpose AI model: it uses Whisper, SigLIP 2 and Apple's and Microsoft's models, and calls
the language models of the provider each user or site chooses.

- **Prohibited practices (art. 5), high risk (art. 6, Annex III):** none of rushbin's uses is among them.
- **No emotion recognition and no biometric categorisation (art. 3(39), 3(40), 50(3)).** rushbin does not recognize
  faces, identify people, read emotions, or put people into categories. Apple's classifier has classes that sort
  people by age or body feature (`adult`, `baby`, `child`, `teen`, `tattoo`), the kind of categories recital 16
  names: rushbin drops them (`personLabels` in `vision.go` and `macos/vision.swift`). Classes like `people` or
  `crowd` say that people are in the frame, not which category a person belongs to. The voice filter's classes
  only give one number per sentence, whether someone speaks or sings; nothing about who is kept.
- **Art. 50(1), interaction with people:** does not apply. rushbin has no chatbot; the Commission's guidelines on
  Article 50 (C(2026) 7702, 5 November 2026) list transcription tools and back-end decision support where the user
  only sees the output among the systems outside it (section 3.1.1).
- **Art. 50(2), marking synthetic content:** in the guidelines' reading, transcriptions of conversations do not
  alter the meaning of the input (examples after paragraph 92), data labels and short outputs are out of scope
  (paragraph 68), and so are outputs that only reproduce existing content (paragraph 65) or are not meant for
  people (paragraph 67: the search vectors). The tidy plan is a JSON list of moves shown only to the person who
  asked for it. So the marking is not owed; rushbin marks transcripts and looks anyway (above), because a
  transcript that reads like a quote should never pass for one.
- **Art. 50(4), deep fakes and published text:** rushbin generates no images, audio or video. A team that publishes
  a transcript to inform the public is the deployer of that text; a transcript marked checked has been reviewed by
  a person of the team.
- **Art. 4, AI literacy** (from 27 July 2026 as rewritten by Regulation 2026/1744: take measures to support it):
  this page, the labels in the app and on the site, and the terms, EULA and privacy policy, which say that
  transcripts, on-screen text and labels are automatic and may be wrong.

Sources: Regulation (EU) 2024/1689, EUR-Lex CELEX 32024R1689 (art. 3, 4, 5, 50, 113, recital 16); Regulation (EU)
2026/1744, OJ L 24.7.2026 (art. 4 replaced; art. 111(4): providers already on the market comply with art. 50(2) by
2 December 2026); Commission guidelines on Article 50, C(2026) 7702 final; Code of Practice on marking and labelling
AI-generated content, 10 June 2026 (voluntary).
