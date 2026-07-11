# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

During v0.x, breaking changes are allowed but must be listed under a
**Breaking** heading, and every removed/renamed configuration key or API
must fail fast with an error message that embeds the new form (the error
text is the migration guide).

## [Unreleased]

### Breaking

- Component and `use: model` step `prompt` now renders as the **system**
  message (persona/instruction), with the step input as the user message —
  it was previously sent as the user message. When the input is empty, the
  rendered prompt degrades back to a user message, so prompt-only calls keep
  working unchanged.
- Prompts that reference undeclared placeholders are now rejected at
  assembly time (placeholders must be declared `params` or reserved `$`
  variables).
- Renamed configuration keys — the old key fails fast at assembly with an
  error that embeds the new form:
  - `loop.max_steps` → `max_rounds` (the semantics were always rounds);
  - `engine_config.step_max_steps` → `step_max_rounds`;
  - `memory.tools` → `expose_tools`;
  - `work_dir` → `state_dir` (writable runtime state only; read-only
    resources move to the resource loader);
  - namespace-level `tools:` (source declarations) → `sources:`;
  - external skill `use:` (fetch link) → `from:`.

### Added

- Unified input model: `{$user_input}` reserved variable (the user's
  original message, constant across nesting), step-level `input:` (rendered
  in the caller's scope, becomes the callee's user message and re-sets its
  scoped `{$input}`), and multi-stage engines now pass params plus built-in
  variables into stage prompts.
- Feishu channel: inbound `post` rich-text messages are parsed (text nodes
  joined), covering topic-thread @-mentions.
- Finish guard: completion-notice-only final answers are detected and the
  prior substantive deliverable is spliced back deterministically.
- Store hardening: mutating-effect journal is two-phase (an "in-flight"
  marker lands before execution; journal unavailable ⇒ refuse to execute),
  and pending-turn pickup is an atomic claim (read-and-delete in one
  mutation, safe across replicas).

Initial public release preparation:

- Apache-2.0 license, CI gate (build / race tests / vet / gofmt / layering
  guard), community docs.
- Declarative multi-file configuration (app / agents / namespaces) with
  assembly-time fail-fast validation.
- Capability protocol (`cap://`) with registries for models, tools, skills,
  engines, stores, sessions, memory, channels, secrets, exec sandboxes,
  decorators, and redis clients.
- Ring-0 loop guarantees: approval interception, durable effect idempotency,
  oversized-result digestion, structured progress events.
- Suspendable human-in-the-loop ("offload and replay"): ask_user/approval
  waits survive process restarts and replica switches, over both IM channels
  and stateless HTTP.
- Feishu (Lark) channel: long-connection mode, thread-aware sessions, card
  lifecycle with third-party outbound decorators.
- Gateway: HTTP messages API (JSON/SSE), A2A protocol, IM webhooks.
