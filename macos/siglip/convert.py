# Converts the image half of SigLIP 2 base to Core ML for rushbin-vision, and checks it against PyTorch.
#   python convert.py OUT.mlpackage THUMBS_DIR   (macos/siglip/build-siglip.sh sets up the pinned versions)
import sys, glob, numpy as np, torch, coremltools as ct
from PIL import Image
from transformers import AutoModel, AutoProcessor

out, thumbs = sys.argv[1], sys.argv[2]
name = "google/siglip2-base-patch16-256"
model = AutoModel.from_pretrained(name, torch_dtype=torch.float32, attn_implementation="eager").eval()  # sdpa does not convert
proc = AutoProcessor.from_pretrained(name)


class Image2Vec(torch.nn.Module):
    """pixels in [-1, 1], 256×256 → the normalized image embedding (768).
    The pooling head is written out with its own weights and fixed sizes: torch.nn.MultiheadAttention computes
    its shapes at run time, which coremltools cannot convert (aten::Int on a tensor)."""
    def __init__(self, m):
        super().__init__()
        self.m = m

    def forward(self, x):
        vm = self.m.vision_model
        h = vm.post_layernorm(vm.encoder(inputs_embeds=vm.embeddings(x)).last_hidden_state)  # (1, 256, 768)
        head, mha = vm.head, vm.head.attention
        E, nh = 768, mha.num_heads
        hd = E // nh
        W, b = mha.in_proj_weight, mha.in_proj_bias
        q = head.probe @ W[:E].T + b[:E]                              # (1, 1, 768)
        k = h @ W[E:2 * E].T + b[E:2 * E]                             # (1, 256, 768)
        v = h @ W[2 * E:].T + b[2 * E:]
        q = q.reshape(1, 1, nh, hd).transpose(1, 2)                   # (1, nh, 1, hd)
        k = k.reshape(1, 256, nh, hd).transpose(1, 2)
        v = v.reshape(1, 256, nh, hd).transpose(1, 2)
        a = torch.softmax(q @ k.transpose(2, 3) / hd ** 0.5, dim=-1)
        o = (a @ v).transpose(1, 2).reshape(1, 1, E) @ mha.out_proj.weight.T + mha.out_proj.bias
        o = o + head.mlp(head.layernorm(o))
        return torch.nn.functional.normalize(o[:, 0], dim=-1)


def reference(x):
    with torch.no_grad():
        v = model.get_image_features(pixel_values=x)
        return torch.nn.functional.normalize(v.pooler_output if hasattr(v, "pooler_output") else v, dim=-1)


net = Image2Vec(model).eval()
example = torch.rand(1, 3, 256, 256) * 2 - 1
with torch.no_grad():
    print("rewritten head vs transformers:", float((net(example) * reference(example)).sum()))
traced = torch.jit.trace(net, example)
ml = ct.convert(
    traced,
    inputs=[ct.ImageType(name="image", shape=(1, 3, 256, 256), scale=1 / 127.5, bias=[-1, -1, -1], color_layout=ct.colorlayout.RGB)],
    outputs=[ct.TensorType(name="embedding")],
    compute_precision=ct.precision.FLOAT16,
    minimum_deployment_target=ct.target.macOS13,
)
ml.short_description = "SigLIP 2 base (google/siglip2-base-patch16-256, Apache-2.0): image embedding, 768, normalized"
ml.save(out)

# Same frames through PyTorch (the processor) and Core ML (the image squashed to 256×256, bilinear, like the processor does).
paths = sorted(glob.glob(thumbs + "/*/3.jpg"))[:20]
sims = []
for p in paths:
    img = Image.open(p).convert("RGB")
    with torch.no_grad():
        a = net(proc(images=[img], return_tensors="pt")["pixel_values"]).numpy()[0]
    b = ml.predict({"image": img.resize((256, 256), Image.BILINEAR)})["embedding"][0]
    sims.append(float(np.dot(a, b) / np.linalg.norm(b)))
print(f"cosine PyTorch vs Core ML on {len(sims)} frames: min {min(sims):.4f}, mean {np.mean(sims):.4f}")
