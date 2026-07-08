# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

During v0.x, breaking changes are allowed but must be listed under a
**Breaking** heading, and every removed/renamed configuration key or API
must fail fast with an error message that embeds the new form (the error
text is the migration guide).

## [Unreleased]

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
