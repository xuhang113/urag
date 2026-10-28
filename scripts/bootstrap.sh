#!/usr/bin/env bash
# 拉起本机 MySQL / Milvus / Elasticsearch，初始化模型、台账和索引，然后跑测试。
# 已有同名容器时直接复用，不删除数据，不启动收录，不停止其他进程。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

export URAG_EMBED_MODEL_DIR="${URAG_EMBED_MODEL_DIR:-$HOME/.cache/urag-models/Qwen3-Embedding-4B}"
export URAG_RERANK_MODEL_DIR="${URAG_RERANK_MODEL_DIR:-$HOME/.cache/urag-models/bge-reranker-base}"
export URAG_MILVUS_ADDR="${URAG_MILVUS_ADDR:-127.0.0.1:19530}"
export URAG_ELASTIC_ADDR="${URAG_ELASTIC_ADDR:-http://127.0.0.1:9200}"
export URAG_MYSQL_ADDR="${URAG_MYSQL_ADDR:-127.0.0.1:3306}"
export URAG_MYSQL_DATABASE="${URAG_MYSQL_DATABASE:-urag}"

if ! command -v docker >/dev/null 2>&1; then
  echo "需要 Docker" >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  echo "需要 Go" >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "需要 python3" >&2
  exit 1
fi

container_state() {
  if docker ps --format '{{.Names}}' | grep -qx "$1"; then
    echo running
    return
  fi
  if docker ps -a --format '{{.Names}}' | grep -qx "$1"; then
    echo stopped
    return
  fi
  echo absent
}

ensure_running() {
  local name="$1"
  shift
  local state
  state="$(container_state "$name")"
  if [ "$state" = running ]; then
    echo "复用容器 $name"
    return
  fi
  if [ "$state" = stopped ]; then
    echo "启动已有容器 $name"
    docker start "$name" >/dev/null
    return
  fi
  echo "创建容器 $name"
  "$@"
}

wait_tcp() {
  local host="$1"
  local port="$2"
  local i=0
  while [ "$i" -lt 30 ]; do
    if python3 -c 'import socket,sys; s=socket.create_connection((sys.argv[1], int(sys.argv[2])), 2); s.close()' "$host" "$port"; then
      return 0
    fi
    i=$((i + 1))
    sleep 2
  done
  echo "端口未就绪 ${host}:${port}" >&2
  return 1
}

wait_http() {
  local url="$1"
  local i=0
  while [ "$i" -lt 30 ]; do
    if curl -sf -o /dev/null --max-time 2 "$url"; then
      return 0
    fi
    i=$((i + 1))
    sleep 2
  done
  echo "地址未就绪" >&2
  return 1
}

create_mysql() {
  if [ -z "${URAG_MYSQL_ROOT_PASSWORD:-}" ]; then
    echo "新建 urag-mysql 需要环境变量 URAG_MYSQL_ROOT_PASSWORD。已有容器则不用设置。" >&2
    exit 1
  fi
  docker run -d --name urag-mysql \
    --restart unless-stopped \
    -p 127.0.0.1:3306:3306 \
    -e MYSQL_DATABASE="$URAG_MYSQL_DATABASE" \
    -e MYSQL_ROOT_PASSWORD="$URAG_MYSQL_ROOT_PASSWORD" \
    -v urag-mysql-data:/var/lib/mysql \
    mysql:8.4 >/dev/null
}

create_milvus() {
  docker run -d --name urag-milvus \
    -p 127.0.0.1:19530:19530 \
    -e ETCD_USE_EMBED=true \
    -e ETCD_CONFIG_PATH=/milvus/configs/embedEtcd.yaml \
    -e ETCD_DATA_DIR=/var/lib/milvus/etcd \
    -e COMMON_STORAGETYPE=local \
    -v urag-milvus-data:/var/lib/milvus \
    -v "$ROOT/deploy/embedEtcd.yaml:/milvus/configs/embedEtcd.yaml:ro" \
    milvusdb/milvus:v2.4.17 \
    milvus run standalone >/dev/null
}

create_elastic() {
  if ! docker image inspect urag-es-smartcn:8.17.4 >/dev/null 2>&1; then
    echo "构建带 smartcn 的 Elasticsearch 镜像"
    docker build -t urag-es-smartcn:8.17.4 -f "$ROOT/deploy/elasticsearch.Dockerfile" "$ROOT/deploy"
  fi
  docker run -d --name urag-elastic \
    --user 1000:0 \
    -p 127.0.0.1:9200:9200 \
    -e discovery.type=single-node \
    -e xpack.security.enabled=false \
    -e xpack.security.enrollment.enabled=false \
    -e xpack.security.http.ssl.enabled=false \
    -e "ES_JAVA_OPTS=-Xms1g -Xmx1g" \
    -v urag-elastic-data:/usr/share/elasticsearch/data \
    --entrypoint /bin/tini \
    urag-es-smartcn:8.17.4 \
    -- /usr/local/bin/docker-entrypoint.sh eswrapper >/dev/null
}

ensure_running urag-mysql create_mysql
ensure_running urag-milvus create_milvus
ensure_running urag-elastic create_elastic

mysql_host="${URAG_MYSQL_ADDR%%:*}"
mysql_port="${URAG_MYSQL_ADDR##*:}"
wait_tcp "$mysql_host" "$mysql_port"

if [ "$URAG_MILVUS_ADDR" = "127.0.0.1:19530" ]; then
  wait_tcp 127.0.0.1 19530
fi
if [ "$URAG_ELASTIC_ADDR" = "http://127.0.0.1:9200" ]; then
  wait_http "http://127.0.0.1:9200"
fi

python3 "$ROOT/scripts/init-models.py"
"$ROOT/scripts/init-db.sh"
"$ROOT/scripts/init-es.sh"
go test ./...

echo "一键部署完成。未启动收录。"
