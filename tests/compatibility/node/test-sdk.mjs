import assert from 'node:assert/strict';
import OpenAI from 'openai';

const baseURL = process.env.HEIMDALL_COMPAT_BASE_URL ?? 'http://127.0.0.1:18088/v1';
const apiKey = 'gw_sdk_compatibility';
const client = new OpenAI({ apiKey, baseURL, maxRetries: 0, timeout: 5_000 });

const completion = await client.chat.completions.create({
  model: 'compat-chat',
  messages: [{ role: 'user', content: 'ping' }],
});
assert.equal(completion.choices[0].message.content, 'compat-ok');
assert.equal(completion.usage.total_tokens, 5);

const embedding = await client.embeddings.create({ model: 'compat-embedding', input: 'ping' });
assert.deepEqual(embedding.data[0].embedding, [0.125, -0.25, 0.5]);
assert.equal(embedding.usage.total_tokens, 2);

const stream = await client.chat.completions.create({
  model: 'compat-chat',
  messages: [{ role: 'user', content: 'ping' }],
  stream: true,
  stream_options: { include_usage: true },
});
let content = '';
let argumentsJSON = '';
let totalTokens;
for await (const chunk of stream) {
  if (chunk.usage) totalTokens = chunk.usage.total_tokens;
  for (const choice of chunk.choices) {
    content += choice.delta.content ?? '';
    for (const call of choice.delta.tool_calls ?? []) {
      argumentsJSON += call.function?.arguments ?? '';
    }
  }
}
assert.equal(content, 'compat-ok');
assert.deepEqual(JSON.parse(argumentsJSON), { value: 'ok' });
assert.equal(totalTokens, 7);

for (const [model, status] of [['error-budget', 403], ['error-rate', 429], ['error-provider', 502]]) {
  await assert.rejects(
    client.chat.completions.create({ model, messages: [{ role: 'user', content: 'ping' }] }),
    (error) => error instanceof OpenAI.APIError && error.status === status,
  );
}
