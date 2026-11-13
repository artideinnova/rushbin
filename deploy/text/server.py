# The text half of SigLIP 2 base (google/siglip2-base-patch16-256, Apache-2.0): turns the words of a search
# into the vector the Macs' image embeddings are compared with. POST /embed {"q": "..."} → {"v": [768 numbers]}.
# Only reachable by the rushbin container (no published port). About 20 ms a search on 2 CPU threads.
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
from transformers import AutoModel, AutoProcessor

NAME = "google/siglip2-base-patch16-256"
torch.set_num_threads(2)
model = AutoModel.from_pretrained(NAME).eval()
del model.vision_model  # images are embedded on the Macs
proc = AutoProcessor.from_pretrained(NAME)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/embed":
            self.send_error(404)
            return
        n = min(int(self.headers.get("Content-Length", 0)), 4096)
        q = str(json.loads(self.rfile.read(n) or b"{}").get("q", ""))[:300]
        x = proc(text=[q], padding="max_length", max_length=64, truncation=True, return_tensors="pt")
        with torch.no_grad():
            v = model.get_text_features(**x)
        v = v.pooler_output if hasattr(v, "pooler_output") else v
        v = torch.nn.functional.normalize(v, dim=-1)[0].tolist()
        body = json.dumps({"v": v}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):  # searches are not logged
        pass


ThreadingHTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
