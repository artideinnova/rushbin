#!/bin/zsh
# Builds rushbin.app (universal: Apple silicon + Intel).
#   macos/build.sh OUT_DIR [FFMPEG_DIR [WHISPER_DIR [SIGLIP_DIR [RED_DIR]]]]
# FFMPEG_DIR holds ffmpeg, ffprobe and COPYING.LGPLv2.1 from build-ffmpeg.sh; WHISPER_DIR holds whisper-cli
# and WHISPER-LICENSE.txt from build-whisper.sh; SIGLIP_DIR holds SigLIP2Image.mlpackage from siglip/build-siglip.sh.
# Without them the app uses the ones on the PATH (development only), and has no search by content.
# RED_DIR holds rushbin-red and RED's REDR3D.dylib and REDMetal.dylib (R3D decoded on the GPU) (from the private rushbin-red repository: RED's SDK license
# keeps it out of this one); without it R3D files are cataloged as plain files.
# Sparkle (the updater, MIT) is fetched once into OUT_DIR/sparkle, from its release checked against its SHA-256;
# Publishing the dmg signs it for Sparkle with sign_update, from the Sparkle folder of OUT.
# SIGN_ID=<Developer ID hash> signs with the hardened runtime; NOTARY_PROFILE=<notarytool profile> also
# makes a notarized, stapled rushbin-VERSION.dmg.
set -eu
ROOT=${0:A:h:h}
OUT=${1:?usage: build.sh OUT_DIR [FFMPEG_DIR [WHISPER_DIR [SIGLIP_DIR [RED_DIR]]]]}; FF=${2:-}; WH=${3:-}; SG=${4:-}; RD=${5:-}
VERSION=${VERSION:-0.1.0}
FINAL=$OUT/rushbin.app
APP=$OUT/new/rushbin.app # built aside: a failed build leaves the previous app working
rm -rf $OUT/new; mkdir -p $APP/Contents/MacOS $APP/Contents/Resources $APP/Contents/Frameworks
cd $ROOT
SPARKLE=2.10.1
SPARKLE_SHA256=1a856d2a9edb8e628de88121d7be04cf0e2167f8641650b78fca00b14244cf27
SP=$OUT/sparkle
if [[ $(plutil -extract CFBundleShortVersionString raw $SP/Sparkle.framework/Resources/Info.plist 2>/dev/null) != $SPARKLE ]]; then
  rm -rf $SP $SP.new $OUT/sparkle.tar.xz && mkdir $SP.new
  curl -fsSL -o $OUT/sparkle.tar.xz https://github.com/sparkle-project/Sparkle/releases/download/$SPARKLE/Sparkle-$SPARKLE.tar.xz
  [[ ${$(shasum -a 256 $OUT/sparkle.tar.xz)[1]} == $SPARKLE_SHA256 ]] || { echo "Sparkle-$SPARKLE.tar.xz is not the release: wrong SHA-256"; exit 1 }
  tar -xf $OUT/sparkle.tar.xz -C $SP.new && rm $OUT/sparkle.tar.xz && mv $SP.new $SP
fi
for a in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH=$a go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o $OUT/engine-$a .
done
lipo -create $OUT/engine-arm64 $OUT/engine-amd64 -output $APP/Contents/MacOS/rushbin-engine
for a in arm64 x86_64; do
  swiftc -O -target $a-apple-macos13.0 -F $SP -Xlinker -rpath -Xlinker @executable_path/../Frameworks macos/main.swift -o $OUT/shell-$a
  swiftc -O -target $a-apple-macos13.0 macos/vision.swift -o $OUT/vision-$a
done
lipo -create $OUT/shell-arm64 $OUT/shell-x86_64 -output $APP/Contents/MacOS/rushbin
lipo -create $OUT/vision-arm64 $OUT/vision-x86_64 -output $APP/Contents/MacOS/rushbin-vision
rm -f $OUT/engine-* $OUT/shell-* $OUT/vision-*
sed "s/__VERSION__/$VERSION/g" macos/Info.plist > $APP/Contents/Info.plist
cp macos/AppIcon.icns $APP/Contents/Resources/
cp -R macos/*.lproj $APP/Contents/Resources/ # the shell's words and the permission prompts, per language
cp LICENSE.md NOTICE.md $APP/Contents/Resources/
# The licenses of the Go modules inside the engine, as their MIT and BSD licenses ask of a program shipped built.
{
  print -r -- "== Go standard library"$'\n'; cat $(go env GOROOT)/LICENSE; print
  go version -m $APP/Contents/MacOS/rushbin-engine | awk '$1 == "dep" { print $2 }' | while read -r m; do
    print -r -- "== $m"$'\n'; cat $(go list -m -f '{{.Dir}}' $m)/LICENSE; print
  done
} > $APP/Contents/Resources/GO-LICENSES.txt
FW=$APP/Contents/Frameworks/Sparkle.framework
ditto $SP/Sparkle.framework $FW
rm -rf $FW/XPCServices $FW/Versions/B/XPCServices # only for sandboxed apps, and rushbin is not (Sparkle's docs)
cp $SP/LICENSE $APP/Contents/Resources/SPARKLE-LICENSE.txt
if [[ -n $FF ]]; then
  cp $FF/ffmpeg $FF/ffprobe $APP/Contents/MacOS/
  cp $FF/COPYING.LGPLv2.1 $APP/Contents/Resources/FFMPEG-LICENSE.txt
fi
if [[ -n $WH ]]; then
  cp $WH/whisper-cli $APP/Contents/MacOS/
  cp $WH/WHISPER-LICENSE.txt $APP/Contents/Resources/
fi
[[ -n $SG ]] && cp -R $SG/SigLIP2Image.mlpackage macos/siglip/SIGLIP-LICENSE.txt $APP/Contents/Resources/
[[ -n $RD ]] && cp $RD/rushbin-red $RD/REDR3D.dylib $RD/REDMetal.dylib $APP/Contents/MacOS/
if [[ -n ${SIGN_ID:-} ]]; then
  for f in $APP/Contents/MacOS/*; do
    case $f in
      */MacOS/rushbin|*/MacOS/REDR3D.dylib|*/MacOS/REDMetal.dylib) ;; # the app is signed below; RED's libraries keep RED's own signature, as shipped
      */MacOS/rushbin-red) codesign --force --options runtime --timestamp --entitlements macos/red.entitlements -s $SIGN_ID $f ;;
      *) codesign --force --options runtime --timestamp -s $SIGN_ID $f ;;
    esac
  done
  for f in $FW/Versions/B/Autoupdate $FW/Versions/B/Updater.app $FW; do # inside out, as Sparkle's docs sign it
    codesign --force --options runtime --timestamp -s $SIGN_ID $f
  done
  codesign --force --options runtime --timestamp -s $SIGN_ID $APP
  codesign --verify --strict --deep $APP
fi
rm -rf $FINAL && mv $APP $FINAL && rmdir $OUT/new && APP=$FINAL
if [[ -n ${NOTARY_PROFILE:-} ]]; then
  DMG=$OUT/rushbin-$VERSION.dmg
  rm -f $DMG
  hdiutil create -quiet -volname rushbin -srcfolder $APP -ov -format UDZO $DMG
  codesign --force --timestamp -s $SIGN_ID $DMG
  xcrun notarytool submit $DMG --keychain-profile $NOTARY_PROFILE --wait
  xcrun stapler staple $DMG
  spctl --assess --type open --context context:primary-signature -v $DMG
fi
echo "built $APP ($(lipo -archs $APP/Contents/MacOS/rushbin-engine))"
