# Text encoder for search by content (deploy/text/server.py). The model is fetched at build time.
FROM python:3.12-slim
RUN pip install --no-cache-dir torch==2.15.0 --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir transformers==5.22.1 sentencepiece protobuf pillow # the processor loads its image half too
ENV HF_HOME=/models HF_HUB_OFFLINE=0
RUN python -c "from transformers import AutoModel, AutoProcessor; n='google/siglip2-base-patch16-256'; AutoModel.from_pretrained(n); AutoProcessor.from_pretrained(n)"
ENV HF_HUB_OFFLINE=1
COPY server.py /server.py
USER 65534
EXPOSE 8000
CMD ["python", "/server.py"]
