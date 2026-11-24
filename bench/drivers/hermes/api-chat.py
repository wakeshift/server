# A chat client of Hermes's OpenAI-compatible API server, as an app that
# uses Hermes's API would be (wakebench's hermes-api lane, drivers/hermes/api.ts).
# It runs inside Hermes's container and reads one message per line from the
# terminal, POSTs it to /v1/chat/completions with the gateway's API key and
# X-Hermes-Session-Id (so Hermes keeps the conversation's history, and the
# session is one Wakeshift's plugin can wake), and prints the reply.
#
# Turns Hermes starts in the session (a woken run, /v1/runs) aren't shown:
# an app sees them only if it reads the session's history, as this one
# doesn't. Python's standard library only; the key is read from the
# environment and never printed.
#
# Usage: python api-chat.py <session id>

import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request

BUSY = "waiting for Hermes's API server"


def wait_ready(base: str) -> None:
    """Waits for the API server's /health (the gateway starts it after its platforms)."""
    end = time.time() + 150
    while time.time() < end:
        try:
            with urllib.request.urlopen(base + "/health", timeout=5) as r:
                if r.status == 200:
                    return
        except Exception:
            pass
        time.sleep(1)
    # No prompt: the chat didn't start (the bench reads it as such).
    sys.exit("Hermes's API server didn't answer /health in 150 s")


def spin(stop: threading.Event) -> None:
    frames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
    t0 = time.time()
    i = 0
    while not stop.wait(0.5):
        sys.stdout.write(f"\r{frames[i % len(frames)]} {BUSY} · {int(time.time() - t0)}s")
        sys.stdout.flush()
        i += 1


def ask(base: str, key: str, session: str, text: str) -> str:
    body = json.dumps({"messages": [{"role": "user", "content": text}]}).encode()
    req = urllib.request.Request(
        base + "/v1/chat/completions",
        data=body,
        method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key, "X-Hermes-Session-Id": session},
    )
    try:
        with urllib.request.urlopen(req, timeout=900) as r:
            doc = json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        return f"error: HTTP {e.code}: {e.read().decode('utf-8', 'replace')[:500]}"
    except Exception as e:  # a timeout, the gateway restarting
        return f"error: {type(e).__name__}: {str(e)[:300]}"
    try:
        return doc["choices"][0]["message"]["content"] or "(empty reply)"
    except (KeyError, IndexError, TypeError):
        return "error: unexpected response: " + json.dumps(doc)[:500]


def main() -> None:
    if len(sys.argv) != 2:
        sys.exit("usage: api-chat.py <session id>")
    session = sys.argv[1]
    key = os.environ.get("API_SERVER_KEY", "")
    if not key:
        sys.exit("API_SERVER_KEY isn't set")
    base = f"http://127.0.0.1:{os.environ.get('API_SERVER_PORT') or 8642}"
    print(f"Hermes API server client: POST /v1/chat/completions, X-Hermes-Session-Id: {session}", flush=True)
    wait_ready(base)
    while True:
        try:
            text = input("> ")
        except (EOFError, KeyboardInterrupt):
            print()
            return
        if not text.strip():
            continue
        stop = threading.Event()
        t = threading.Thread(target=spin, args=(stop,), daemon=True)
        t.start()
        try:
            reply = ask(base, key, session, text)
        except KeyboardInterrupt:
            stop.set()
            print()
            return
        stop.set()
        t.join()
        sys.stdout.write("\r\033[K")
        print(reply.rstrip() + "\n", flush=True)


if __name__ == "__main__":
    main()
