"""Score one local cross-encoder batch, then exit.

This process does not open Hugging Face and does not download weights.
It only reads a sequence-classification model that is already on disk.
The caller supplies the question and the passages. Scores are probabilities.
It does not generate an answer and does not query a database.
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


def note(token):
    raw = os.environ.get("URAG_STEP_FD")
    if not raw:
        return
    try:
        os.write(int(raw), (token + "\n").encode())
    except (OSError, ValueError):
        return


def note_model(name, done):
    name = " ".join(str(name).split())
    if not name:
        return
    token = "rerank_model_done " if done else "rerank_model "
    note(token + name)


def choose_runtime():
    # One device for the whole batch. Prefer MPS on this machine.
    # A failed MPS forward is an error, not a switch to CPU.
    import torch

    if torch.backends.mps.is_available():
        return torch.device("mps"), torch.float16
    if torch.cuda.is_available():
        return torch.device("cuda"), torch.bfloat16
    return torch.device("cpu"), torch.float32


def score_pairs(model, tokenizer, device, query, passages):
    import torch

    values = []
    step = 16
    for start in range(0, len(passages), step):
        group = passages[start : start + step]
        features = tokenizer(
            [[query, passage] for passage in group],
            padding=True,
            truncation=True,
            max_length=512,
            return_tensors="pt",
        )
        features = {key: value.to(device) for key, value in features.items()}
        with torch.no_grad():
            logits = model(**features).logits.float()
            logits = logits.view(logits.shape[0], -1)
            if logits.shape[-1] == 1:
                picked = torch.sigmoid(logits[:, 0])
            else:
                picked = torch.softmax(logits, dim=-1)[:, -1]
            values.extend(picked.tolist())
    return values


def score_batch(req):
    model_dir = req["model_dir"]
    passages = req.get("passages")
    query = req.get("query")
    if not isinstance(query, str) or not isinstance(passages, list) or not passages:
        raise SystemExit("rerank batch is empty")
    name = req.get("model") or "cross-encoder"
    note_model(name, False)

    import torch
    from transformers import AutoModelForSequenceClassification, AutoTokenizer
    from transformers.utils import logging as hf_logging

    hf_logging.set_verbosity_error()
    device, dtype = choose_runtime()
    tokenizer = AutoTokenizer.from_pretrained(model_dir, local_files_only=True)
    model = AutoModelForSequenceClassification.from_pretrained(
        model_dir,
        local_files_only=True,
        torch_dtype=dtype,
        low_cpu_mem_usage=True,
    )
    model.to(device)
    model.eval()
    note("load_rerank_done")
    values = score_pairs(model, tokenizer, device, query, passages)
    if len(values) != len(passages):
        raise SystemExit("score count does not match the batch")
    note("rerank_done")
    note_model(name, True)
    return {"scores": values}


def main():
    real_stdout = sys.stdout
    sys.stdout = sys.stderr
    try:
        req = json.load(sys.stdin)
        if req.get("task") != "rerank":
            sys.stderr.write("cross encoder rejects a non-rerank task\n")
            raise SystemExit(2)
        result = score_batch(req)
    except SystemExit:
        raise
    except Exception as exc:
        sys.stderr.write("cross encoder failed: %s\n" % exc)
        raise SystemExit(1) from exc
    finally:
        sys.stdout = real_stdout
    json.dump(result, real_stdout)
    real_stdout.write("\n")
    real_stdout.flush()


if __name__ == "__main__":
    main()
