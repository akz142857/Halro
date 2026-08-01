import json
import os
import time
import urllib.request

import openai
import anthropic


BASE_URL = os.environ.get("HEIMDALL_COMPAT_BASE_URL", "http://127.0.0.1:18088/v1")
API_KEY = "gw_sdk_compatibility"


def client(api_key: str = API_KEY) -> openai.OpenAI:
    return openai.OpenAI(api_key=api_key, base_url=BASE_URL, max_retries=0, timeout=5.0)


def test_anthropic_messages() -> None:
    sdk = anthropic.Anthropic(api_key=API_KEY, base_url=BASE_URL.removesuffix("/v1"), max_retries=0, timeout=5.0)
    message = sdk.messages.create(model="compat-chat", max_tokens=32, messages=[{"role": "user", "content": "ping"}])
    assert message.content[0].text == "compat-ok"
    assert message.usage.output_tokens == 2
    text = ""
    with sdk.messages.stream(model="compat-chat", max_tokens=32, messages=[{"role": "user", "content": "ping"}]) as stream:
        for value in stream.text_stream:
            text += value
    assert text == "compat-ok"


def test_non_stream_and_embeddings() -> None:
    completion = client().chat.completions.create(
        model="compat-chat", messages=[{"role": "user", "content": "ping"}]
    )
    assert completion.choices[0].message.content == "compat-ok"
    assert completion.usage is not None and completion.usage.total_tokens == 5

    embedding = client().embeddings.create(model="compat-embedding", input="ping")
    assert embedding.data[0].embedding == [0.125, -0.25, 0.5]
    assert embedding.usage.total_tokens == 2

    response = client().responses.create(model="compat-chat", input="ping", store=False)
    assert response.output_text == "compat-ok"
    assert response.usage is not None and response.usage.total_tokens == 5
    assert response.store is False


def test_responses_stream() -> None:
    stream = client().responses.create(model="compat-chat", input="ping", store=False, stream=True)
    content = ""
    completed = False
    for event in stream:
        if event.type == "response.output_text.delta":
            content += event.delta
        if event.type == "response.completed":
            completed = True
    assert content == "compat-ok"
    assert completed


def test_stream_tool_calls_and_usage_only_chunk() -> None:
    stream = client().chat.completions.create(
        model="compat-chat",
        messages=[{"role": "user", "content": "ping"}],
        stream=True,
        stream_options={"include_usage": True},
    )
    content = ""
    arguments = ""
    total_tokens = None
    for chunk in stream:
        if chunk.usage is not None:
            total_tokens = chunk.usage.total_tokens
        for choice in chunk.choices:
            content += choice.delta.content or ""
            for call in choice.delta.tool_calls or []:
                arguments += call.function.arguments or ""
    assert content == "compat-ok"
    assert json.loads(arguments) == {"value": "ok"}
    assert total_tokens == 7


def test_error_matrix() -> None:
    cases = [("error-budget", 403), ("error-rate", 429), ("error-provider", 502)]
    for model, status in cases:
        try:
            client().chat.completions.create(
                model=model, messages=[{"role": "user", "content": "ping"}]
            )
            raise AssertionError(f"{model} unexpectedly succeeded")
        except openai.APIStatusError as exc:
            assert exc.status_code == status
    try:
        client("gw_invalid").chat.completions.create(
            model="compat-chat", messages=[{"role": "user", "content": "ping"}]
        )
        raise AssertionError("invalid key unexpectedly succeeded")
    except openai.AuthenticationError as exc:
        assert exc.status_code == 401


def test_disconnecting_stream_releases_server_resources() -> None:
    stream = client().chat.completions.create(
        model="slow-stream",
        messages=[{"role": "user", "content": "ping"}],
        stream=True,
    )
    next(iter(stream))
    stream.close()
    stats_url = BASE_URL.removesuffix("/v1") + "/compat/stats"
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        with urllib.request.urlopen(stats_url, timeout=1) as response:
            stats = json.load(response)
        if stats["active_streams"] == 0 and stats["canceled_streams"] >= 1:
            return
        time.sleep(0.05)
    raise AssertionError(f"stream resources were not released: {stats}")


if __name__ == "__main__":
    # Official Anthropic SDKs exercise x-api-key and anthropic-version headers.
    test_anthropic_messages()
    test_non_stream_and_embeddings()
    # Keep the executable CI entry point aligned with pytest discovery.
    test_responses_stream()
    test_stream_tool_calls_and_usage_only_chunk()
    test_error_matrix()
    test_disconnecting_stream_releases_server_resources()
