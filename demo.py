#!/usr/bin/env python3
"""Two-session demo: full-history replay vs memory-layer /context.

Session 1 — agent has no memory store yet. Sends the growing chat history to
Bedrock every turn, then POSTs each user turn to /ingest so facts land in Redis.

Session 2 — "restart the agent". No history is resent. GET /context pulls the
compressed facts for this user_id; only those + the new question go to Bedrock.

Prints latency and token counts side by side so the recording shows the delta live.
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

import boto3

MEMORY_URL = os.getenv("MEMORY_URL", "http://127.0.0.1:8080")
MODEL = os.getenv("BEDROCK_MODEL", "anthropic.claude-3-haiku-20240307-v1:0")
REGION = os.getenv("AWS_REGION", os.getenv("AWS_DEFAULT_REGION", "us-east-1"))
USER_ID = "demo-user"

# A short preference arc: enough history that replay is visibly expensive,
# and a clear fact (/ingest) that session 2 must recall without the transcript.
SESSION1_TURNS = [
    "Hi — I'm Alex. I'm planning weeknight dinners for my family.",
    "Important: I'm vegetarian and allergic to peanuts. Please never suggest dishes with nuts.",
    "We usually have about 30 minutes to cook after work.",
    "What should I make tonight that fits those constraints?",
]

SESSION2_QUESTION = (
    "I'm back. Suggest a different dinner for tonight — same constraints as before."
)

ASSISTANT_SYSTEM = (
    "You are a concise dinner assistant. Obey dietary constraints. "
    "Reply in at most 3 short sentences."
)


def http_json(method: str, url: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        url,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"} if body is not None else {},
    )
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.loads(resp.read().decode())


def bedrock_chat(bedrock, system: str, messages: list[dict]) -> tuple[str, int, int, float]:
    """Invoke Claude on Bedrock; return (text, input_tokens, output_tokens, latency_ms)."""
    payload = {
        "anthropic_version": "bedrock-2023-05-31",
        "max_tokens": 256,
        "temperature": 0.2,
        "system": system,
        "messages": messages,
    }
    t0 = time.perf_counter()
    raw = bedrock.invoke_model(
        modelId=MODEL,
        contentType="application/json",
        accept="application/json",
        body=json.dumps(payload),
    )
    ms = (time.perf_counter() - t0) * 1000
    resp = json.loads(raw["body"].read())
    text = resp["content"][0]["text"].strip()
    usage = resp.get("usage") or {}
    return text, int(usage.get("input_tokens", 0)), int(usage.get("output_tokens", 0)), ms


def fmt_row(label: str, in_tok: int, out_tok: int, ms: float, extra: str = "") -> str:
    total = in_tok + out_tok
    base = f"  {label:<28} in={in_tok:<5} out={out_tok:<4} total={total:<5}  {ms:7.0f} ms"
    return base + (f"  {extra}" if extra else "")


def main() -> int:
    print("=" * 72)
    print("Mini Memory Layer demo — full history vs retrieved context")
    print(f"  memory service: {MEMORY_URL}")
    print(f"  bedrock model:  {MODEL}")
    print(f"  user_id:        {USER_ID}")
    print("=" * 72)

    try:
        http_json("GET", f"{MEMORY_URL}/health")
    except urllib.error.URLError as e:
        print(f"memory service not reachable: {e}", file=sys.stderr)
        return 1

    bedrock = boto3.client("bedrock-runtime", region_name=REGION)

    # --- Session 1: replay full history every turn, ingest after each user turn ---
    print("\n── Session 1: store preference (full history → Bedrock, then /ingest) ──\n")
    history: list[dict] = []
    s1_in = s1_out = 0
    s1_ms = 0.0
    last_tin = last_tout = 0
    last_ms = 0.0
    stored_facts: list[str] = []

    for turn in SESSION1_TURNS:
        history.append({"role": "user", "content": turn})
        reply, tin, tout, ms = bedrock_chat(bedrock, ASSISTANT_SYSTEM, history)
        history.append({"role": "assistant", "content": reply})
        s1_in += tin
        s1_out += tout
        s1_ms += ms
        last_tin, last_tout, last_ms = tin, tout, ms
        print(fmt_row("session1 turn", tin, tout, ms, f'user="{turn[:48]}…"'))
        print(f"    assistant: {reply}")

        ing = http_json(
            "POST",
            f"{MEMORY_URL}/ingest",
            {"user_id": USER_ID, "role": "user", "message": turn},
        )
        if ing.get("stored"):
            stored_facts.append(ing["fact"])
            print(f"    /ingest stored ({ing['latency_ms']} ms): {ing['fact']}")
        else:
            print(f"    /ingest skipped ({ing['latency_ms']} ms): {ing.get('fact')!r}")

    s1_prompt_chars = sum(len(m["content"]) for m in history)

    # --- "Restart the agent": drop local history; Redis still holds mem:{user_id} ---
    print("\n── Agent restart (local history cleared; Redis facts persist) ──\n")
    history.clear()
    print(f"  local history messages: {len(history)}")
    print(f"  facts kept in Redis for {USER_ID}: {len(stored_facts)}")
    for f in stored_facts:
        print(f"    • {f}")

    # --- Session 2: /context instead of resending history ---
    print("\n── Session 2: recall via /context (no history replay) ──\n")

    q = urllib.parse.urlencode({"user_id": USER_ID, "query": SESSION2_QUESTION, "k": "3"})
    ctx = http_json("GET", f"{MEMORY_URL}/context?{q}")
    print(f"  /context ({ctx['latency_ms']} ms) — {ctx['total_stored']} stored, top-{len(ctx['facts'])}:")
    for f in ctx["facts"]:
        print(f"    • {f}")

    memory_block = ctx["context"] or "(no memories)"
    s2_messages = [
        {
            "role": "user",
            "content": (
                f"Known facts about this user:\n{memory_block}\n\n"
                f"User question: {SESSION2_QUESTION}"
            ),
        }
    ]
    reply2, tin2, tout2, ms2 = bedrock_chat(bedrock, ASSISTANT_SYSTEM, s2_messages)
    print(fmt_row("session2 (with memory)", tin2, tout2, ms2))
    print(f"    assistant: {reply2}")

    s2_prompt_chars = len(s2_messages[0]["content"])

    # --- Side-by-side delta (what the recording needs) ---
    # Headline compares the last session-1 call (full transcript replay) to
    # session-2 (compact /context) — same question shape, different memory path.
    saved_pct = 0.0
    if last_tin > 0:
        saved_pct = max(0.0, 100.0 * (1.0 - (tin2 / last_tin)))

    print("\n" + "=" * 72)
    print("TOKEN / LATENCY DELTA")
    print("=" * 72)
    print(fmt_row("session1 last turn (replay)", last_tin, last_tout, last_ms,
                  f"history_msgs={len(SESSION1_TURNS)*2} prompt_chars≈{s1_prompt_chars}"))
    print(fmt_row("session2 with /context", tin2, tout2, ms2,
                  f"prompt_chars≈{s2_prompt_chars}"))
    print(fmt_row("session1 cumulative", s1_in, s1_out, s1_ms, "(all replay turns)"))
    print()
    print(f"  input-token savings (session2 vs session1 last turn): {saved_pct:.0f}%")
    print(f"  latency: session2 {ms2:.0f} ms vs session1 last turn {last_ms:.0f} ms")
    print()
    print("  Hook: same conversation, far fewer tokens on turn two —")
    print("  because the memory layer remembered instead of replaying.")
    print("=" * 72)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as e:  # noqa: BLE001 — demo script: surface the real blocker
        print(f"\nDEMO FAILED: {e}", file=sys.stderr)
        raise
