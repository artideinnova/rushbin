#!/bin/zsh
# Builds the whisper-cli bundled in the Mac app (whisper.cpp, MIT): static, Metal shaders embedded,
# no third-party libraries, universal (Apple silicon + Intel). The model is not bundled: the app downloads
# it once and checks its SHA-256 (speech.go).
#   macos/build-whisper.sh OUT_DIR
set -eu
V=v1.9.5
COMMIT=d1be6fde11ac6e0407606b4e42fe72d34add8037 # the commit $V points to, checked after the clone
OUT=${1:?usage: build-whisper.sh OUT_DIR}
mkdir -p $OUT && cd $OUT
[[ -d whisper.cpp ]] || git clone -q --depth 1 --branch $V https://github.com/ggml-org/whisper.cpp
[[ $(git -C whisper.cpp rev-parse HEAD) == $COMMIT ]] || { echo "whisper.cpp $V is not commit $COMMIT"; exit 1; }
cmake -S whisper.cpp -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
  -DCMAKE_OSX_ARCHITECTURES="arm64;x86_64" -DCMAKE_OSX_DEPLOYMENT_TARGET=13.0 \
  -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON -DGGML_NATIVE=OFF -DGGML_BLAS=OFF \
  -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF -DWHISPER_SDL2=OFF > cmake.log
cmake --build build --config Release -j$(sysctl -n hw.ncpu) --target whisper-cli > build.log
mkdir -p universal
cp build/bin/whisper-cli universal/
cp whisper.cpp/LICENSE universal/WHISPER-LICENSE.txt
echo "whisper.cpp $V: $(lipo -archs universal/whisper-cli)"
otool -L universal/whisper-cli | tail -n +2
