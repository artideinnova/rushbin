# Converts the text half of SigLIP 2 base to Core ML for search by looks on a Mac in no team (search.go), and
# checks it against PyTorch. Not bundled in the app: the result is published on Hugging Face
# (artideinnova/siglip2-base-patch16-256-text-coreml) and downloaded by the app on first need.
#   python convert-text.py OUT_DIR QUERIES.json   (the pinned versions of build-siglip.sh; QUERIES: testdata/siglip-text.json)
# OUT_DIR gets SigLIP2Text.mlpackage, SigLIP2Text.mlmodelc.zip (what the app downloads) and tokenizer.model.
import sys, json, shutil, numpy as np, torch, coremltools as ct
from huggingface_hub import hf_hub_download
from transformers import AutoModel, AutoTokenizer

out, queries = sys.argv[1], [q["q"] for q in json.load(open(sys.argv[2]))]
name, rev = "google/siglip2-base-patch16-256", "3f9f96cb90da5dbc758b01813f2f6f1aee24c1ab"
model = AutoModel.from_pretrained(name, revision=rev, torch_dtype=torch.float32, attn_implementation="eager").eval()  # sdpa does not convert
tok = AutoTokenizer.from_pretrained(name, revision=rev)


class Text2Vec(torch.nn.Module):
    """64 token ids (the search, then <eos>, padded with <pad>; no attention mask, as in training) → the
    normalized text embedding (768), comparable with SigLIP2Image's."""
    def __init__(self, m):
        super().__init__()
        self.m = m

    def forward(self, ids):
        return torch.nn.functional.normalize(self.m.text_model(input_ids=ids.long()).pooler_output, dim=-1)


def ids(q):
    return tok([q[:300]], padding="max_length", max_length=64, truncation=True, return_tensors="np")["input_ids"].astype(np.int32)


net = Text2Vec(model).eval()
traced = torch.jit.trace(net, torch.from_numpy(ids(queries[0])))
ml = ct.convert(
    traced,
    inputs=[ct.TensorType(name="ids", shape=(1, 64), dtype=np.int32)],
    outputs=[ct.TensorType(name="embedding")],
    compute_precision=ct.precision.FLOAT16,
    minimum_deployment_target=ct.target.macOS13,
)
ml.short_description = "SigLIP 2 base (google/siglip2-base-patch16-256, Apache-2.0): text embedding, 768, normalized"
ml.save(f"{out}/SigLIP2Text.mlpackage")
shutil.rmtree(f"{out}/SigLIP2Text.mlmodelc", ignore_errors=True)
shutil.move(ct.utils.compile_model(f"{out}/SigLIP2Text.mlpackage"), f"{out}/SigLIP2Text.mlmodelc")
shutil.make_archive(f"{out}/SigLIP2Text.mlmodelc", "zip", out, "SigLIP2Text.mlmodelc")
shutil.copy(hf_hub_download(name, "tokenizer.model", revision=rev), f"{out}/tokenizer.model")

sims = []
for q in queries:
    x = ids(q)
    with torch.no_grad():
        a = net(torch.from_numpy(x)).numpy()[0]
    b = ml.predict({"ids": x})["embedding"][0]
    sims.append(float(np.dot(a, b) / np.linalg.norm(b)))
print(f"cosine PyTorch vs Core ML on {len(sims)} searches: min {min(sims):.5f}, mean {np.mean(sims):.5f}")
