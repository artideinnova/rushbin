#!/bin/zsh
# Builds the ffmpeg and ffprobe bundled in the Mac app: LGPL only (no --enable-gpl, no x264),
# no third-party libraries, hardware encoding through VideoToolbox, universal (Apple silicon + Intel).
#   macos/build-ffmpeg.sh OUT_DIR
set -eu
V=9.0.2
OUT=${1:?usage: build-ffmpeg.sh OUT_DIR}
mkdir -p $OUT && cd $OUT
if [[ ! -d ffmpeg-$V ]]; then
  curl -fsSLO https://ffmpeg.org/releases/ffmpeg-$V.tar.xz
  curl -fsSLO https://ffmpeg.org/releases/ffmpeg-$V.tar.xz.asc
  # FFmpeg release signing key, as published on https://ffmpeg.org/download.html
  export GNUPGHOME=$OUT/gnupg; mkdir -p -m 700 $GNUPGHOME
  curl -fsSL https://ffmpeg.org/ffmpeg-devel.asc | gpg --quiet --import
  gpg --quiet --verify ffmpeg-$V.tar.xz.asc ffmpeg-$V.tar.xz
  tar xf ffmpeg-$V.tar.xz
fi
for arch in arm64 x86_64; do
  [[ -x $arch/bin/ffprobe ]] && continue
  rm -rf build-$arch && mkdir build-$arch && cd build-$arch
  cross=(); [[ $arch != $(uname -m) ]] && cross=(--enable-cross-compile)
  ../ffmpeg-$V/configure --prefix=$OUT/$arch --arch=$arch --target-os=darwin $cross \
    --cc="clang -arch $arch" --extra-cflags="-mmacosx-version-min=13.0" --extra-ldflags="-mmacosx-version-min=13.0" \
    --disable-autodetect --enable-videotoolbox --enable-audiotoolbox --enable-zlib \
    --disable-network --disable-doc --disable-debug --disable-ffplay > configure.log
  make -j$(sysctl -n hw.ncpu) > make.log 2>&1
  make install > /dev/null
  cd ..
done
mkdir -p universal
for b in ffmpeg ffprobe; do lipo -create arm64/bin/$b x86_64/bin/$b -output universal/$b; done
cp ffmpeg-$V/COPYING.LGPLv2.1 universal/
echo "ffmpeg $V: $(lipo -archs universal/ffmpeg)"
otool -L universal/ffmpeg | tail -n +2
