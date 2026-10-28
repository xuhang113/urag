"""One Qwen3-Reranker-4B batch, then exit.

No local reranker model card is available, and this process does not open
Hugging Face. The instruction matches the embedding model card already on disk:
Instruct: Given a web search query, retrieve relevant passages that answer the query
Query: is followed immediately by the question, with no space.
Go puts that question and the caller-supplied passage into each pair.
This process only reads yes/no probabilities. It does not generate an answer,
does not query a database, and does not download weights.
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


def note(token):
    # 试查把阶段描述符放在 URAG_STEP_FD。没有这个变量时不写任何记号。
    raw = os.environ.get("URAG_STEP_FD")
    if not raw:
        return
    try:
        os.write(int(raw), (token + "\n").encode())
    except (OSError, ValueError):
        return


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
    # One device for the whole batch. Prefer MPS on this machine.
    # A failed MPS forward is an error, not a switch to CPU.
    import torch

    if torch.backends.mps.is_available():
        return torch.device("mps"), torch.float16
    if torch.cuda.is_available():
        return torch.device("cuda"), torch.bfloat16
    return torch.device("cpu"), torch.float32


def row_groups(rows, budget):
    group = []
    used = 0
    for row in rows:
        width = len(row)
        if group and used + width > budget:
            yield group
            group = []
            used = 0
        group.append(row)
        used += width
    if group:
        yield group


def score_batch(req):
    model_dir = req["model_dir"]
    pairs = req.get("pairs")
    if not isinstance(pairs, list) or not pairs:
        raise SystemExit("rerank batch is empty")
    require_hidden(model_dir)
    name = req.get("model") or "Qwen/Qwen3-Reranker-4B"
    name = " ".join(str(name).split())
    if name:
        note("rerank_model " + name)

    import torch
    from transformers import AutoModelForCausalLM, AutoTokenizer
    from transformers.utils import logging as hf_logging

    hf_logging.set_verbosity_error()
    device, dtype = choose_runtime()
    tokenizer = AutoTokenizer.from_pretrained(model_dir, local_files_only=True)
    tokenizer.padding_side = "left"
    model = AutoModelForCausalLM.from_pretrained(
        model_dir,
        local_files_only=True,
        torch_dtype=dtype,
        low_cpu_mem_usage=True,
    )
    model.to(device)
    model.eval()
    model.config.use_cache = False
    note("load_rerank_done")

    prefix = (
        "<|im_start|>system\n"
        "Judge whether the Document meets the requirements based on the Query and the Instruct provided. "
        'Note that the answer can only be "yes" or "no".<|im_end|>\n'
        "<|im_start|>user\n"
    )
    suffix = "<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n"
    prefix_tokens = tokenizer.encode(prefix, add_special_tokens=False)
    suffix_tokens = tokenizer.encode(suffix, add_special_tokens=False)
    keep = 8192 - len(prefix_tokens) - len(suffix_tokens)
    if keep < 8:
        raise SystemExit("rerank template longer than the context")
    token_true = tokenizer.convert_tokens_to_ids("yes")
    token_false = tokenizer.convert_tokens_to_ids("no")
    unk = getattr(tokenizer, "unk_token_id", None)
    if token_true == token_false or token_true == unk or token_false == unk:
        raise SystemExit("yes/no tokens are missing, refusing to guess")
    inputs = tokenizer(
        list(pairs),
        padding=False,
        truncation="longest_first",
        return_attention_mask=False,
        max_length=keep,
    )
    rows = []
    for token_ids in inputs["input_ids"]:
        rows.append(prefix_tokens + list(token_ids) + suffix_tokens)
    # 一次前向只留大约两千个 token。五十条长段落垫成同一批时，
    # 词表矩阵和注意力都大到 MPS 内核完不成。模型仍然只加载一次。
    values = []
    for group in row_groups(rows, 2048):
        features = tokenizer.pad(
            {"input_ids": group},
            padding=True,
            return_tensors="pt",
        )
        features = {key: value.to(device) for key, value in features.items()}
        with torch.no_grad():
            # 左边填充时，最后一个位置就是真正的末 token。输出层只打这一列。
            hidden = model.model(**features).last_hidden_state[:, -1, :]
            last = model.lm_head(hidden)
            true_vector = last[:, token_true]
            false_vector = last[:, token_false]
            stacked = torch.stack([false_vector, true_vector], dim=1)
            picked = torch.nn.functional.log_softmax(stacked.float(), dim=1).exp()[:, 1]
            values.extend(picked.tolist())
    if len(values) != len(pairs):
        raise SystemExit("score count does not match the batch")
    note("rerank_done")
    if name:
        note("rerank_model_done " + name)
    return {"scores": values}


def main():
    real_stdout = sys.stdout
    sys.stdout = sys.stderr
    try:
        req = json.load(sys.stdin)
        if req.get("task") != "rerank":
            sys.stderr.write("qwen rerank script rejects a non-rerank task\n")
            raise SystemExit(2)
        result = score_batch(req)
    except SystemExit:
        raise
    except Exception as exc:
        sys.stderr.write("qwen rerank failed: %s\n" % exc)
        raise SystemExit(1) from exc
    finally:
        sys.stdout = real_stdout
    json.dump(result, real_stdout)
    real_stdout.write("\n")
    real_stdout.flush()


if __name__ == "__main__":
    main()
