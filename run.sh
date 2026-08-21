#!/usr/bin/env bash
# One-command demo: Redis → Go memory service → 2-session Bedrock comparison.
set -euo pipefail
cd "$(dirname "$0")"

REDIS_NAME=mem0-mini-redis
MEMORY_PORT=8080
REDIS_ADDR=127.0.0.1:6379
MEMORY_PID=""

need_docker() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi
  if sg docker -c 'docker info' >/dev/null 2>&1; then
    exec sg docker -c "\"$0\" $*"
  fi
  echo "Docker is required (and your user must reach the docker socket)." >&2
  exit 1
}

cleanup() {
  [[ -n "${MEMORY_PID}" ]] && kill "${MEMORY_PID}" 2>/dev/null || true
  docker rm -f "${REDIS_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

need_docker "$@"

if ! command -v aws >/dev/null 2>&1; then
  echo "aws CLI required" >&2
  exit 1
fi
if ! aws sts get-caller-identity >/dev/null 2>&1; then
  echo "AWS credentials required for Bedrock (aws sts get-caller-identity failed)." >&2
  echo "Configure a profile/role that can invoke Bedrock in \$AWS_REGION (default us-east-1)." >&2
  exit 1
fi

export AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
export AWS_DEFAULT_REGION="$AWS_REGION"
export BEDROCK_MODEL="${BEDROCK_MODEL:-anthropic.claude-3-haiku-20240307-v1:0}"

echo "==> Redis (mem:{user_id} list schema)"
docker rm -f "${REDIS_NAME}" >/dev/null 2>&1 || true
docker run -d --name "${REDIS_NAME}" -p 6379:6379 redis:7-alpine >/dev/null
for i in $(seq 1 40); do
  if docker exec "${REDIS_NAME}" redis-cli PING 2>/dev/null | grep -q PONG; then
    break
  fi
  sleep 0.25
done
docker exec "${REDIS_NAME}" redis-cli PING | grep -q PONG

echo "==> Go modules + build memory service"
go mod tidy
CGO_ENABLED=0 go build -o memory-service .

echo "==> Python venv (boto3 for Bedrock chat turns)"
if [[ ! -d .venv ]]; then
  python3 -m venv .venv
  .venv/bin/pip install -q -r requirements.txt
fi

echo "==> Start memory service :${MEMORY_PORT}"
REDIS_ADDR="${REDIS_ADDR}" LISTEN=":${MEMORY_PORT}" \
  BEDROCK_MODEL="${BEDROCK_MODEL}" AWS_REGION="${AWS_REGION}" \
  ./memory-service > /tmp/mem0-memory.log 2>&1 &
MEMORY_PID=$!

for i in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${MEMORY_PORT}/health" >/dev/null; then
    break
  fi
  sleep 0.25
done
if ! curl -sf "http://127.0.0.1:${MEMORY_PORT}/health" >/dev/null; then
  echo "memory service failed to start:" >&2
  cat /tmp/mem0-memory.log >&2 || true
  exit 1
fi

echo "==> Run 2-session demo (full history vs /context)"
MEMORY_URL="http://127.0.0.1:${MEMORY_PORT}" \
  BEDROCK_MODEL="${BEDROCK_MODEL}" AWS_REGION="${AWS_REGION}" \
  .venv/bin/python demo.py

echo
echo "Redis key after demo:"
docker exec "${REDIS_NAME}" redis-cli LRANGE mem:demo-user 0 -1
