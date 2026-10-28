#!/usr/bin/env python3
"""下载并校验 Qwen3-Embedding-4B 与 BAAI/bge-reranker-base。

目录来自环境变量，否则用文档中的默认路径：
  URAG_EMBED_MODEL_DIR  默认 ~/.cache/urag-models/Qwen3-Embedding-4B
  URAG_RERANK_MODEL_DIR 默认 ~/.cache/urag-models/bge-reranker-base

重排权重 config 里的 _name_or_path 可以是 xlm-roberta-base，那是底座名。
目录名保持 bge-reranker-base。本脚本不打印密钥，也不把权重写入仓库。
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
from pathlib import Path

EMBED_REPO = "Qwen/Qwen3-Embedding-4B"
RERANK_REPO = "BAAI/bge-reranker-base"
RERANK_DIR_NAME = "bge-reranker-base"
EMBED_DIR_NAME = "Qwen3-Embedding-4B"


def main() -> None:
    os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")
    embed_dir = model_dir("URAG_EMBED_MODEL_DIR", EMBED_DIR_NAME)
    rerank_dir = model_dir("URAG_RERANK_MODEL_DIR", RERANK_DIR_NAME)
    if rerank_dir.name == "xlm-roberta-base":
        raise SystemExit(
            "重排目录名必须是 bge-reranker-base。xlm-roberta-base 只是 config 里的底座名。"
        )
    if rerank_dir.name != RERANK_DIR_NAME:
        raise SystemExit(f"重排目录名必须是 {RERANK_DIR_NAME}，当前是 {rerank_dir.name}")
    prepare(EMBED_REPO, embed_dir, qwen_files)
    prepare(RERANK_REPO, rerank_dir, bge_files)
    check_qwen(embed_dir)
    check_bge(rerank_dir)
    print(f"Qwen3-Embedding-4B 已校验 {embed_dir}")
    print(f"bge-reranker-base 已校验 {rerank_dir}（config 底座名 xlm-roberta-base）")


def model_dir(env_name: str, default_name: str) -> Path:
    raw = os.environ.get(env_name, "").strip()
    if not raw:
        raw = str(Path.home() / ".cache" / "urag-models" / default_name)
    return Path(raw).expanduser()


def qwen_files(dest: Path) -> list[str]:
    names = ["config.json", "model.safetensors.index.json", "tokenizer.json"]
    index_path = dest / "model.safetensors.index.json"
    if index_path.is_file():
        names.extend(shard_names(index_path))
    return unique(names)


def bge_files(_dest: Path) -> list[str]:
    return ["config.json", "model.safetensors", "tokenizer.json"]


def prepare(repo: str, dest: Path, names_for) -> None:
    dest.mkdir(parents=True, exist_ok=True)
    meta = fetch_meta(repo)
    # 分片清单在索引文件里。先放清单，再补权重，每个文件只校验一次。
    if repo == EMBED_REPO:
        first = ["config.json", "model.safetensors.index.json", "tokenizer.json"]
    else:
        first = bge_files(dest)
    done = []
    for name in first:
        ensure_file(repo, dest, name, meta)
        done.append(name)
    seen = set(done)
    for name in names_for(dest):
        if name in seen:
            continue
        ensure_file(repo, dest, name, meta)
        seen.add(name)


def ensure_file(repo: str, dest: Path, name: str, meta: dict | None) -> None:
    path = dest / name
    if ".." in Path(name).parts or Path(name).is_absolute():
        raise SystemExit(f"非法文件名 {name}")
    expected = None if meta is None else meta.get(name)
    if expected is not None and path.is_file() and expected[1]:
        print(f"正在校验 {name}")
    if path.is_file() and path.stat().st_size > 0 and matches(path, expected):
        print(f"已校验 {repo} {name}")
        return
    if meta is None and path.is_file() and path.stat().st_size > 0:
        print(f"远端不可用，保留本机文件 {name}", file=sys.stderr)
        return
    if meta is None:
        raise SystemExit(f"缺少 {name}，且远端不可用")
    if meta is not None and name not in meta:
        raise SystemExit(f"{repo} 没有文件 {name}")
    print(f"下载 {repo} {name}")
    download(repo, dest, name)
    if not path.is_file() or path.stat().st_size <= 0 or not matches(path, expected):
        raise SystemExit(f"校验失败 {name}")


def matches(path: Path, expected) -> bool:
    if expected is None:
        return False
    size, sha = expected
    if size is None and not sha:
        return False
    if size is not None and path.stat().st_size != size:
        return False
    if sha:
        return sha256_file(path) == sha
    return True


def fetch_meta(repo: str) -> dict | None:
    try:
        from huggingface_hub import HfApi, get_hf_file_metadata, hf_hub_url
    except ImportError:
        raise SystemExit("需要 huggingface_hub。可以执行 python3 -m pip install huggingface_hub")
    try:
        api = HfApi()
        try:
            info = api.model_info(repo, files_metadata=True)
        except TypeError:
            info = api.model_info(repo)
        out = {}
        for sibling in info.siblings or []:
            size = getattr(sibling, "size", None)
            sha = None
            lfs = getattr(sibling, "lfs", None)
            if lfs is not None:
                sha = getattr(lfs, "sha256", None) or None
                size = getattr(lfs, "size", None) or size
            if not sha:
                size, sha = head_sha(hf_hub_url, get_hf_file_metadata, repo, sibling.rfilename, size)
            out[sibling.rfilename] = (size, normalize_sha(sha))
        if not out:
            return None
        return out
    except Exception as exc:
        print(f"远端清单不可用：{type(exc).__name__}", file=sys.stderr)
        return None


def head_sha(hf_hub_url, get_hf_file_metadata, repo: str, name: str, size):
    try:
        meta = get_hf_file_metadata(hf_hub_url(repo_id=repo, filename=name))
    except Exception:
        return size, None
    got = meta.size or size
    etag = normalize_sha(getattr(meta, "etag", None))
    return got, etag


def normalize_sha(value) -> str | None:
    if not value:
        return None
    text = str(value).strip()
    if text.startswith("W/"):
        text = text[2:].strip()
    text = text.strip('"').lower()
    if len(text) == 64 and all(c in "0123456789abcdef" for c in text):
        return text
    return None


def download(repo: str, dest: Path, name: str) -> None:
    from huggingface_hub import hf_hub_download

    hf_hub_download(repo_id=repo, filename=name, local_dir=str(dest))


def shard_names(index_path: Path) -> list[str]:
    raw = json.loads(index_path.read_text(encoding="utf-8"))
    mapping = raw.get("weight_map") or {}
    if not mapping:
        raise SystemExit(f"{index_path} 没有 weight_map")
    names = []
    for name in mapping.values():
        if not name or "/" in name or "\\" in name or ".." in name:
            raise SystemExit(f"非法分片 {name}")
        names.append(name)
    return names


def check_qwen(dest: Path) -> None:
    cfg = read_config(dest)
    if cfg.get("model_type") != "qwen3" or cfg.get("hidden_size") != 2560:
        raise SystemExit("Qwen3-Embedding-4B 的 config 不是 hidden_size 2560 的 qwen3")
    arch = cfg.get("architectures") or []
    if "Qwen3ForCausalLM" not in arch:
        raise SystemExit("Qwen3-Embedding-4B 的架构不符")
    if not (dest / "tokenizer.json").is_file():
        raise SystemExit("Qwen3-Embedding-4B 缺少分词器")
    for name in shard_names(dest / "model.safetensors.index.json"):
        path = dest / name
        if not path.is_file() or path.stat().st_size < 1_000_000:
            raise SystemExit(f"Qwen3-Embedding-4B 缺少权重 {name}")


def check_bge(dest: Path) -> None:
    if dest.name != RERANK_DIR_NAME:
        raise SystemExit(f"重排目录名必须是 {RERANK_DIR_NAME}")
    cfg = read_config(dest)
    if cfg.get("model_type") != "xlm-roberta":
        raise SystemExit("bge-reranker-base 的 model_type 应为 xlm-roberta")
    if cfg.get("architectures") != ["XLMRobertaForSequenceClassification"]:
        raise SystemExit("bge-reranker-base 应是 sequence-classification")
    base = cfg.get("_name_or_path")
    if base not in (None, "xlm-roberta-base", RERANK_REPO, RERANK_DIR_NAME):
        raise SystemExit(f"未识别的重排底座名 {base}")
    path = dest / "model.safetensors"
    if not path.is_file() or path.stat().st_size < 1_000_000:
        raise SystemExit("bge-reranker-base 缺少 model.safetensors")
    if not (dest / "tokenizer.json").is_file():
        raise SystemExit("bge-reranker-base 缺少分词器")


def read_config(dest: Path) -> dict:
    path = dest / "config.json"
    if not path.is_file():
        raise SystemExit(f"缺少 {path}")
    return json.loads(path.read_text(encoding="utf-8"))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while True:
            block = handle.read(1024 * 1024)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def unique(names: list[str]) -> list[str]:
    seen = set()
    out = []
    for name in names:
        if name in seen:
            continue
        seen.add(name)
        out.append(name)
    return out


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except Exception as exc:
        print(type(exc).__name__, file=sys.stderr)
        raise SystemExit(1)
