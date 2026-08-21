# Mini Memory Layer: extract-compress-retrieve service

Memory persistence, compression, and user_id-scoped retrieval that reduces token cost and latency, mirroring Mem0's stated core mechanism.

**Live demo:** https://mem0.ashanpraba.com

The demo runs entirely in the browser against seeded data — no API keys,
no accounts, and no external services required.

## Stack

- Go
- Redis
- AWS Bedrock
- Python (client/demo script)
- Docker

## How it works

- Run Redis locally in Docker; design a simple key schema: mem:{user_id} -> list of compact fact strings.
- Write a small Go HTTP service with POST /ingest (takes a chat turn, calls Bedrock to extract a 1-2 sentence fact, pushes to Redis) and GET /context?user_id=&query= (pulls stored facts, does simple keyword/embedding filter, returns top-k as compact context).
- Write a Python script that simulates a 2-session conversation: session 1 sends full history to Bedrock and calls /ingest after; session 2 calls /context instead of resending history and prints token counts for both approaches.
- Print/log latency and token-count deltas side by side in the terminal for the recording.
- Record a 60-90s take: show session 1 storing a preference, restart the 'agent', show session 2 recalling it via /context with the token savings printed live.

## Running locally

```bash
cd src
bash run.sh
```

Then open the printed URL. A prebuilt static version of the UI lives in
`src/web/` and can be opened directly with no server.
