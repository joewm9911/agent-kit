# Contributing to agent-kit

Thanks for your interest. This project has strong architectural opinions;
this document is the contract. PRs that follow it get merged fast.

## Design principles (normative)

1. **Layering is enforced by machine, not by convention.**
   `core → protocol → runtime → capabilities (agent/skill/todo/askuser) →
   serving → config/std`. Lower layers never import upper layers; nothing
   outside `impl/` imports `impl/`. `scripts/layering-check.sh` runs in CI
   and must pass.

2. **Code registers, config enables by name.**
   Every extension point is a registry (`Register*` + lookup by name in
   YAML). Implementations self-register in `init()` from `impl/...`;
   the assembly layer resolves names and fails fast on unknowns. Never
   add a switch statement where a registry exists.

3. **Discipline lives in the harness, not in the model or the caller.**
   Anything the loop must guarantee (approval, idempotent effects, result
   digestion, progress emission) is a Ring-0 gate wrapped around the tool
   surface — the model cannot opt out. If a guarantee can be skipped by a
   prompt or a caller mistake, it is not a guarantee.

4. **Two kinds of errors.**
   Errors a model can fix are returned as tool results (self-correction);
   errors that must abort the turn implement `TurnTerminal()` and pierce
   every fallback middleware. Never convert one into the other silently.

5. **Fail fast at assembly, and the error text is the migration guide.**
   Configuration mistakes are rejected at build time, and any removed or
   renamed key must produce an error that embeds the new spelling with an
   example. Runtime is too late; a vague error is a bug.

6. **Live verification for model-facing changes.**
   Changes to prompts, engines, or model adapters require a real-model
   A/B run (`SMOKE_LIVE=1`, see `config/engine_live_test.go`). Replay
   fixtures prove wiring, not model behavior. Report the live results in
   the PR description.

7. **Internal unity, boundary translation.**
   One vocabulary inside the framework (see `docs/config-taxonomy.md`);
   translate at the edges when an external protocol (eino, IM platforms)
   uses different words.

## Language policy

- Error messages and exported-symbol doc comments: **English**.
- Implementation comments and internal design docs: any language
  (much of the existing internals is commented in Chinese; do not
  mass-translate, and do not let it block your contribution).
- User-visible strings in `serving` are configurable text; defaults are
  English (IM deployments override them per channel).

## Practical checklist

Before opening a PR:

```sh
gofmt -l . && go vet ./...
go test ./... -race -count=1
bash scripts/layering-check.sh
```

- New extension implementations go under `impl/<kind>/<name>/` and
  self-register; add a table row to `docs/extending.md`.
- New configuration keys follow the vocabulary in `docs/config-taxonomy.md`
  (one word, one meaning; `use`/`from`/`sources`/`args`/`params`/`prompt`).
- Registry names from third-party packages should be prefixed
  (`vendor/name`) to avoid collisions with built-ins.
- Breaking changes: update CHANGELOG under **Breaking**, and make the old
  form fail fast with the new form embedded in the error.
- Secrets never enter the repo, tests, or logs — no exceptions. Tests that
  need credentials read environment variables and skip when absent.

## Commit style

Imperative subject, body explains *why* (the decision and its boundary),
not a diff narration. Chinese or English both fine.
