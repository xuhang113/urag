"""Keep Qwen3-Embedding-4B loaded and embed one batch per stdin line.

The caller passes texts unchanged. Document text has no instruction.
Query text is already embed.QueryText:
Instruct: Given a web search query, retrieve relevant passages that answer the query
Query: is followed immediately by the question, with no space.
This process does not add that prefix, does not download weights, and does not
write the original text anywhere.

Each stdin line is one JSON object:
{"task":"embed","model_dir":"...","dim":2560,"texts":["..."]}
Each stdout line is {"vectors":[[...]]}.
The model loads on the first batch and stays on the device chosen once.
Stdin EOF exits. A failed forward exits instead of moving the batch to CPU.
"""

import json
import os
import sys

os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"
os.environ["HF_DATASETS_OFFLINE"] = "1"
os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"
os.environ["HF_HUB_DISABLE_PROGRESS_BARS"] = "1"
os.environ["TOKENIZERS_PARALLELISM"] = "false"

HIDDEN = 2560


def require_hidden(model_dir):
    path = os.path.join(model_dir, "config.json")
    try:
        with open(path, encoding="utf-8") as handle:
            cfg = json.load(handle)
    except FileNotFoundError:
        sys.stderr.write("config.json missing, refusing to download\n")
        raise SystemExit(2)
    hidden = int(cfg.get("hidden_size") or 0)
    if hidden != HIDDEN:
        sys.stderr.write(
            "hidden_size %s != %s, refusing to download or change dimension\n"
            % (hidden, HIDDEN)
        )
        raise SystemExit(2)


def choose_runtime():
    # Pick one device before the forward. This machine prefers MPS.
    # Do not move the batch to CPU if MPS fails halfway through.
    import torch

    if torch.backends.mps.is_available():
        return torch.device("mps"), torch.float16
    if torch.cuda.is_available():
        return torch.device("cuda"), torch.bfloat16
    return torch.device("cpu"), torch.float32


def last_token_pool(last_hidden_states, attention_mask):
    import torch

    left_padding = bool(attention_mask[:, -1].sum().item() == attention_mask.shape[0])
    if left_padding:
        return last_hidden_states[:, -1]
    sequence_lengths = attention_mask.sum(dim=1) - 1
    batch = last_hidden_states.shape[0]
    return last_hidden_states[
        torch.arange(batch, device=last_hidden_states.device),
        sequence_lengths.to(last_hidden_states.device),
    ]


def load_model(model_dir):
    require_hidden(model_dir)

    import torch
    from transformers import AutoModel, AutoTokenizer
    from transformers.utils import logging as hf_logging

    hf_logging.set_verbosity_error()
    device, dtype = choose_runtime()
    tokenizer = AutoTokenizer.from_pretrained(model_dir, local_files_only=True)
    tokenizer.padding_side = "left"
    model = AutoModel.from_pretrained(
        model_dir,
        local_files_only=True,
        torch_dtype=dtype,
        low_cpu_mem_usage=True,
    )
    model.to(device)
    model.eval()
    model.config.use_cache = False
    return model, tokenizer, device


def embed_texts(model, tokenizer, device, texts):
    import torch

    batch = tokenizer(
        list(texts),
        padding=True,
        truncation=True,
        max_length=8192,
        return_tensors="pt",
    )
    batch = {key: value.to(device) for key, value in batch.items()}
    with torch.no_grad():
        hidden = model(**batch).last_hidden_state
        pooled = last_token_pool(hidden, batch["attention_mask"])
        width = int(pooled.shape[-1])
        if width != HIDDEN:
            raise SystemExit(
                "vector width %s != 2560, refusing to change dimension" % width
            )
        # float() stays on the chosen device. It is not a move onto CPU.
        pooled = torch.nn.functional.normalize(pooled.float(), p=2, dim=1)
        return pooled.tolist()


def write_vectors(real_stdout, vectors):
    real_stdout.write(json.dumps({"vectors": vectors}))
    real_stdout.write("\n")
    real_stdout.flush()


def main():
    real_stdout = sys.stdout
    sys.stdout = sys.stderr
    model = None
    tokenizer = None
    device = None
    loaded_dir = None
    try:
        while True:
            raw = sys.stdin.readline()
            if raw == "":
                break
            line = raw.strip()
            if not line:
                continue
            req = json.loads(line)
            if req.get("task") != "embed":
                sys.stderr.write("qwen embed script rejects a non-embed task\n")
                raise SystemExit(2)
            texts = req.get("texts")
            if not isinstance(texts, list) or not texts:
                raise SystemExit("empty embed batch")
            if int(req.get("dim") or 0) != HIDDEN:
                raise SystemExit("dim is not 2560, refusing to change dimension")
            model_dir = req["model_dir"]
            if model is None:
                model, tokenizer, device = load_model(model_dir)
                loaded_dir = model_dir
            elif model_dir != loaded_dir:
                raise SystemExit("model directory changed, refusing to reload")
            vectors = embed_texts(model, tokenizer, device, texts)
            if len(vectors) != len(texts):
                raise SystemExit("vector count mismatch")
            write_vectors(real_stdout, vectors)
    except SystemExit:
        raise
    except Exception as exc:
        sys.stderr.write("qwen embed failed: %s\n" % exc)
        raise SystemExit(1) from exc
    finally:
        sys.stdout = real_stdout


if __name__ == "__main__":
    main()
