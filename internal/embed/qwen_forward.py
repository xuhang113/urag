"""One Qwen3-Embedding-4B batch, then exit.

The caller passes texts unchanged. Document text has no instruction.
Query text is already embed.QueryText:
Instruct: Given a web search query, retrieve relevant passages that answer the query
Query: is followed immediately by the question, with no space.
This process does not add that prefix, does not download weights, and does not
write the original text anywhere.
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


def embed_batch(req):
    model_dir = req["model_dir"]
    texts = req["texts"]
    if not isinstance(texts, list) or not texts:
        raise SystemExit("empty embed batch")
    if int(req["dim"]) != HIDDEN:
        raise SystemExit("dim is not 2560, refusing to change dimension")
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
        vectors = pooled.tolist()
    return {"vectors": vectors}


def main():
    real_stdout = sys.stdout
    sys.stdout = sys.stderr
    try:
        req = json.load(sys.stdin)
        if req.get("task") != "embed":
            sys.stderr.write("qwen embed script rejects a non-embed task\n")
            raise SystemExit(2)
        result = embed_batch(req)
    except SystemExit:
        raise
    except Exception as exc:
        sys.stderr.write("qwen embed failed: %s\n" % exc)
        raise SystemExit(1) from exc
    finally:
        sys.stdout = real_stdout
    json.dump(result, real_stdout)
    real_stdout.write("\n")
    real_stdout.flush()


if __name__ == "__main__":
    main()
