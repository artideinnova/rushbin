#!/bin/zsh
# Makes the preview folders of the SigLIP parity test (vision_test.go): neutral frames drawn by FFmpeg, at the
# size of rushbin's previews, and their reference emb.bin and look line (mac.jsonl) from the Mac app's helper.
#   testdata/vision/make.sh /path/to/rushbin.app
set -eu
APP=${1:?usage: make.sh /path/to/rushbin.app}
cd ${0:A:h}
rm -rf -- *(/N) mac.jsonl
font=/System/Library/Fonts/Supplemental/Arial.ttf
frames() { mkdir -p $1 && ffmpeg -nostdin -loglevel error -f lavfi -i "$2" -frames:v $3 -q:v 2 $1/%d.jpg; }
for src in mandelbrot testsrc2 smptehdbars gradients life cellauto; do frames $src "$src=s=480x270:r=1" 5; done
frames blank "color=c=0x3c3c3c:s=480x270:r=1" 2
frames text "color=c=0x2a4d69:s=480x270:r=1,drawtext=fontfile=${font}:text='Search by content':fontsize=40:fontcolor=0xf0f0f0:x=(w-tw)/2:y=90,drawtext=fontfile=${font}:text='rushbin 2026':fontsize=32:fontcolor=0xf0f0f0:x=(w-tw)/2:y=160" 3
cache=$(mktemp -d)
for d in */; do print -r -- "${d%/}	both"; done |
  $APP/Contents/MacOS/rushbin-vision $APP/Contents/Resources/SigLIP2Image.mlpackage $cache/SigLIP2Image.mlmodelc | sort > mac.jsonl
rm -rf $cache
