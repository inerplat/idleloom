import json
import sys
import time

from mlx_lm import generate, load


def emit(value):
    sys.stdout.write(json.dumps(value, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main():
    if len(sys.argv) != 3 or len(sys.argv[2]) != 64:
        raise SystemExit("usage: runner.py MODEL_DIRECTORY EXECUTION_NONCE")
    model, tokenizer = load(sys.argv[1])
    emit({"type": "ready"})
    for line in sys.stdin:
        request = {}
        try:
            request = json.loads(line)
            request_id = request["id"]
            prompt = request["prompt"]
            max_tokens = request["maxTokens"]
            if not isinstance(request_id, str) or not request_id:
                raise ValueError("id is required")
            if not isinstance(prompt, str) or not prompt or len(prompt.encode("utf-8")) > 16384:
                raise ValueError("prompt must contain 1 to 16384 UTF-8 bytes")
            if not isinstance(max_tokens, int) or max_tokens < 1 or max_tokens > 512:
                raise ValueError("maxTokens must be between 1 and 512")
            started = time.monotonic()
            formatted = prompt
            if getattr(tokenizer, "chat_template", None):
                messages = [{"role": "user", "content": prompt}]
                formatted = tokenizer.apply_chat_template(
                    messages, tokenize=False, add_generation_prompt=True
                )
            output = generate(
                model,
                tokenizer,
                prompt=formatted,
                max_tokens=max_tokens,
                verbose=False,
            )
            emit({
                "type": "result",
                "id": request_id,
                "text": output,
                "elapsedMillis": int((time.monotonic() - started) * 1000),
            })
        except Exception as error:
            emit({
                "type": "error",
                "id": request.get("id", "") if isinstance(request, dict) else "",
                "error": str(error)[:1024],
            })


if __name__ == "__main__":
    main()
