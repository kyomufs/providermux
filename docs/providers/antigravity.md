# Google Antigravity — the `antigravity` provider

The service behind the `antigravity` provider: the model endpoint of Google's
Antigravity CLI, the one the DSH plugin
[`dsh-tool-antigravity`](https://github.com/Jaylor-Wang/dsh-tool-antigravity)
uses.

Every claim below carries its evidence:

- **source** — read from the plugin's tree, `dsh-tool-antigravity` 0.5.3 and the
  package it depends on, `@cortexkit/antigravity-auth-core` 3.0.14. The file and
  the symbol are named beside each claim.
- **measured** — a request sent to the live service from this host, with the date.
  There are none in this file yet: nothing here has been exercised against the
  live service, and no claim below should be read as if it had been.

The provider lives in `internal/provider/antigravity`.

## Why it is not an OpenAI-compatible lane

Three differences make it a provider of the first class rather than a section
with a `base_url`:

| axis | this service | an OpenAI lane |
| --- | --- | --- |
| body | `v1internal` envelope: `{request: {contents, systemInstruction, tools, generationConfig}}` | flat OpenAI body |
| model name | carries the effort: `gemini-3.8-flash-high` | a base name plus a parameter |
| answer | Gemini events: `candidates[].content.parts` | OpenAI chunks |
| credential | OAuth access token minted from a refresh token | a static key |

The first three are the provider's business and live in its package. The fourth
is the only place where the shared machinery is reused in a way worth naming: for
this service a "key" is a **refresh token**, and the access token is derived from
it at send time.

## Routes

| path | method | what it is |
| --- | --- | --- |
| `/v1internal:streamGenerateContent?alt=sse` | POST | the chat route, always in the stream form (`streamPath`) |
| `/v1internal:generateContent` | POST | the single-response twin; not used here |
| `/v1internal:fetchAvailableModels` | POST | the service's own model list; not used, the catalog is a constant |
| `/v1internal:loadCodeAssist` | POST | reports the project of the account; not used, `project` is a setting |

The base URL is `https://daily-cloudcode-pa.googleapis.com` (`DefaultBaseURL`) —
the daily endpoint, which is what the reference client prefers; the production
endpoint `https://cloudcode-pa.googleapis.com` answers the same routes and is its
fallback. `base_url` overrides it.

Source: `src/llm-adapter.ts:76-78` of the plugin (`ANTIGRAVITY_STREAM_ENDPOINT`,
`ANTIGRAVITY_GENERATE_ENDPOINT`), `dist/constants.js` of the auth core for the
daily/production pair.

**The stream form is requested even for a client that asked for one answer.** The
gateway collects the events into a single body itself, so a second, non-streaming
path would be a second shape to keep in step for no gain.

## Credential

A key in this section is **one Google refresh token** — that is, one account.

| what | where it goes |
| --- | --- |
| refresh token | `api_key`, `api_key_env`, or `settings.keys` — the ordinary key pool |
| access token | minted from it, cached per refresh token until 60 s before it expires |
| `Authorization` | `Bearer <access token>` |

The exchange is `POST https://oauth2.googleapis.com/token`, form-encoded:
`client_id`, `client_secret`, `grant_type=refresh_token`, `refresh_token`. The
answer is `{access_token, expires_in, refresh_token?}`. A token the answer does
not date is kept for an hour rather than treated as expired — otherwise every
request would mint one.

The OAuth application defaults to the public one the reference client ships with
(an installed application's secret is not a secret, and the refresh tokens in its
own store were issued to it). A deployment that registers its own application sets
`client_id` and `client_secret`.

Source: `src/credential-coordinator.ts:8-9, 598-640` (endpoint and form),
`dist/constants.js` (`ANTIGRAVITY_CLIENT_ID`, `ANTIGRAVITY_CLIENT_SECRET`).

### Consequences of the key being an account

- `credential` must be `account`: an anonymous call has no account to file a
  request under, and the service refuses it. The configuration check enforces the
  key presence for this mode, which is exactly right here.
- `key_rotation: on` over a pool of refresh tokens is **multi-account failover**:
  a 429 or a 401 on one account is answered from the next, with no code that knows
  what a refresh token is.
- **A refresh token from `dsh-tool-antigravity` can be reused as it is.** The
  plugin stores it at
  `~/.local/share/dsh-tool-antigravity/auth.json` in `refreshToken`, together with
  `projectId`. Both map onto this section: the first is the key, the second is
  `settings.project`. Do not copy the file into the repository — the value is a
  live credential.
- A failed exchange sends **no** `Authorization` header. The service answers 401,
  the gateway reads that as a rejected key and moves on; a token known to be dead
  would produce the same 401 while pretending the account was tried.

## Body

```json
{
  "project": "<settings.project, omitted when empty>",
  "requestId": "agent/<conversation>/<millis>/<trajectory>/1",
  "request": {
    "contents": [{ "role": "user", "parts": [{ "text": "..." }] }],
    "systemInstruction": { "parts": [{ "text": "..." }] },
    "tools": [{ "functionDeclarations": [{ "name": "...", "description": "...", "parameters": { } }] }],
    "labels": {
      "last_step_index": 0,
      "model_enum": "MODEL_PLACEHOLDER_M319",
      "trajectory_id": "...",
      "used_claude": false,
      "used_claude_conservative": false,
      "used_non_gemini_model": false
    },
    "generationConfig": {
      "maxOutputTokens": 65536,
      "temperature": 1,
      "thinkingConfig": { "includeThoughts": true, "thinkingBudget": 4000 }
    },
    "sessionId": "-1234567890123456789"
  },
  "model": "gemini-3.8-flash-medium",
  "userAgent": "antigravity",
  "requestType": "agent"
}
```

Details that are not obvious and are load-bearing:

- **The effort is part of the model name.** There is no `gemini-3.8-flash` route:
  each tier is a route of its own with its own thinking setting. The client's
  `reasoning_effort` is resolved into the route, and an effort the model has no
  route for falls back to the model's default tier.
- **Thinking settings differ per family.** Gemini 3.8 Flash takes
  `thinkingBudget` (1000 / 4000 / −1, where −1 is omitted so the model decides);
  Gemini 4 Argon and both Claude routes take `thinkingLevel`
  (`LOW`/`MEDIUM`/`HIGH`); the Claude routes keep an explicit
  `thinkingBudget: 0` beside the level, because a level alone is read as the
  legacy form and ignored.
- **`sessionId` is the signed FNV-1a 64 hash of the workspace URI**, in decimal.
  It decides the conversation the service files the turns under, so it has to be
  stable across the turns of one workspace; the unsigned reading names a different
  conversation.
- **A tool result is not a message.** The service has two roles, `user` and
  `model`; a result is a `functionResponse` part inside a user turn, and
  consecutive turns of one role are merged. A result whose call is not in the
  conversation is dropped: naming a function the model never called is a refusal.
- **The tool schema is rewritten**: `type` goes upper case, and `$schema`,
  `additionalProperties`, `$id`, `$comment`, `definitions`, `$defs` are dropped —
  the service's protobuf validation refuses them with an error naming a field
  nobody wrote.
- **`functionCall.args` is structure on this wire and a string on the OpenAI one.**
  The two directions are converted in `request.go` and `transcode.go`
  respectively.

Source: `src/llm-adapter.ts` (`buildPayloadFromContents`), `dist/model-registry.js`
(`AGY_NATIVE_MODEL_DEFINITIONS`), `dist/transform/agy-generation.js` and
`dist/transform/gemini.js`.

## Models

The catalog is a constant: the service publishes no list to a client, and this is
the table its own client ships with.

| model (`<provider>/<model>`) | routes | context | output | vision |
| --- | --- | --- | --- | --- |
| `gemini-4-argon` | low / medium / high | 1 048 576 | 65 536 | yes |
| `gemini-3.8-flash` | low / medium / high | 1 048 576 | 65 536 | yes |
| `claude-opus-5-5` | low / medium / high | 1 000 000 | 128 000 | yes |
| `claude-sonnet-5-5` | low / medium / high | 1 000 000 | 128 000 | yes |
| `gpt-oss-120b-medium` | medium | 131 072 | 32 768 | no |

The identifier is the base name; the effort is chosen with `reasoning_effort`, and
the ladder a model advertises is the ladder it has — `gpt-oss-120b-medium` offers
one route and does not pretend to offer three.

Source: `dist/model-registry.js` (`AGY_NATIVE_MODEL_DEFINITIONS`), including the
enum labels each route is filed under.

## Answer

The service streams Gemini events; the client of this gateway reads OpenAI chunks.
The translation happens once, on the body, inside the provider
(`Transcode` in `transcode.go`), and everything downstream — pass-through,
watchdog, collection of a single answer — runs unchanged.

| upstream | OpenAI chunk |
| --- | --- |
| `{text: "..."}` | `delta.content` |
| `{text: "...", thought: true}` | `delta.reasoning_content` |
| `{functionCall: {name, args, id}}` | `delta.tool_calls[{index, id, type: "function", function: {name, arguments: "<json>"}}]` |
| `finishReason: STOP / MAX_TOKENS / SAFETY…` | `stop` / `length` / `content_filter`, and `tool_calls` when calls were sent |
| `usageMetadata` | `usage.prompt_tokens`, `completion_tokens` (candidates + thoughts), `total_tokens`, `prompt_tokens_details.cached_tokens` |

The parts are looked for where the service puts them — `parts`,
`content.parts`, `candidates[].content.parts`, `serverContent.modelTurn.parts` —
and both envelope forms are read (`{"response": {...}}` and the bare object).

Two failure modes are answered rather than hidden:

- an event carrying `.error` ends the reader with an error and **no** `[DONE]`
  marker, so the client sees a truncated answer instead of a short one that looks
  complete;
- a body with no events at all is an error for the same reason.

### Thought signatures

Gemini 3 signs what it produces, and the signature of a function call has to come
back with that call when the client replays the history. Measured on 2026-11-09:
a call answered by `gemini-3.8-flash` carried a 437-character `thoughtSignature`
beside the `functionCall` part, and a replay without it was refused with

```
400 INVALID_ARGUMENT
Function call is missing a thought_signature in functionCall parts. …
Additional data, function call `default_api:skill`, position 2
```

The OpenAI surface has no field for it, so the provider keeps the signatures
itself — keyed by the tool call id, which is the service's own id forwarded as
`tool_calls[].id` — and puts the part field back when the same call comes home
(`signature.go`). A call this process never saw answered, or one evicted from the
bounded store, is replayed with the sentinel the vendor documents for exactly
that case, `skip_thought_signature_validator`. For Claude the sentinel is used
only on the first replayed call, because a run of them there is refused.

A **refusal** is never translated. The status, body and headers reach the client
verbatim, as for every other provider — with the single gateway-wide exception of
`retry-after`, which is capped at a minute before it is handed over
(`refusalHoldMax` in `internal/gateway/gateway.go`); see [Refusals](#refusals).

## Refusals

The provider answers one question in `Classify`: which lever the gateway should
pull, not what to tell the client.

| the service says | verdict | because |
| --- | --- | --- |
| 401, `UNAUTHENTICATED` | key rejected | the token is no longer accepted |
| 429, `RESOURCE_EXHAUSTED` | key rejected | the capacity of the *account* is spent |
| 403, `PERMISSION_DENIED` | key rejected | the account may not call the service |
| 403 naming a location or a country | address rejected, not retried | another account does not fix where the request comes from |
| 503 `UNAVAILABLE`, 504 `DEADLINE_EXCEEDED`, 500 `INTERNAL` | upstream unavailable, retryable | another model of the same provider is another backend |

`retry-after` is read in both spellings (seconds and an HTTP date). It is not,
however, forwarded as it stands: the gateway caps the hold it asks a client to
sit out at one minute, because by the time a refusal reaches the client the
gateway has already rotated keys and spent its own retries. Measured on
2026-11-09: one 429 carried a ten-minute hold, the gateway passed it on verbatim,
and the DSH proxy parked the whole session for those ten minutes.

The wording matched here is the Google error model — `{"error": {"code",
"message", "status"}}` with a gRPC status name. It is **not measured** against
this service; see the note at the top of this file.

## Settings

| key | default | what it does |
| --- | --- | --- |
| `project` | empty | the Google Cloud project the requests are filed under; omitted when empty |
| `user_agent` | the CLI identity | the client identity the service recognises |
| `client_id` | the public application | the OAuth client the refresh tokens were issued to |
| `client_secret` | the public application's | its secret |
| `base_url` | daily endpoint | the service address |

The `User-Agent` is part of the contract rather than decoration: the string is
`antigravity/cli/1.3.5 (aidev_client; os_type=linux; arch=amd64; cl=998863838;
auth_method=consumer)`, and its version and change-list fields are what the
service reads.

Source: `dist/fingerprint.js` (`buildAntigravityHarnessUserAgent`,
`AGY_CLI_VERSION`, `AGY_CLI_CHANGE_LIST`).

## Configuration

```nix
services.providermux = {
  credential = "account";
  key_rotation = "on";

  providers.antigravity = {
    type = "antigravity";
    api_key_env = "ANTIGRAVITY_REFRESH_TOKEN";
    settings.project = "rising-fact-p41fc";
  };

  # A second account is another key, not another section.
  # providers.antigravity.settings.keys = [ "…" "…" ];
};
```

The same section in `config.json`:

```json
{
  "credential": "account",
  "key_rotation": "on",
  "providers": {
    "antigravity": {
      "type": "antigravity",
      "api_key_env": "ANTIGRAVITY_REFRESH_TOKEN",
      "settings": { "project": "rising-fact-p41fc" }
    }
  }
}
```

Then a client asks for `antigravity/claude-opus-5-5` with
`"reasoning_effort": "high"`, and the router resolves the route on its own.
