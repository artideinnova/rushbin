#!/bin/zsh
# Makes SigLIP2Image.mlpackage, the image half of SigLIP 2 base (google/siglip2-base-patch16-256, Apache-2.0)
# for Core ML, bundled in the app for search by content. Pinned versions: coremltools 9 converts PyTorch 2.7.
#   macos/siglip/build-siglip.sh OUT_DIR THUMBS_DIR     (THUMBS_DIR: previews to check the result against PyTorch)
# The text half (convert-text.py, same venv) is not bundled: it is published on Hugging Face and downloaded by the
# app alone (text.go).
set -eu
OUT=${1:?usage: build-siglip.sh OUT_DIR THUMBS_DIR}; THUMBS=${2:?usage: build-siglip.sh OUT_DIR THUMBS_DIR}
HERE=${0:A:h}
mkdir -p $OUT
[[ -x $OUT/venv/bin/python ]] || { uv venv -q $OUT/venv --python 3.12 && uv pip install -q --python $OUT/venv/bin/python \
  torch==2.7.0 transformers==4.51.3 coremltools==9.0 sentencepiece protobuf pillow; }
$OUT/venv/bin/python $HERE/convert.py $OUT/SigLIP2Image.mlpackage $THUMBS
