## What & why

## Checklist

- [ ] `go test ./... -race` and `scripts/layering-check.sh` pass
- [ ] New config keys follow `docs/config-taxonomy.md`; removed/renamed
      keys fail fast with the new form embedded in the error
- [ ] Model-facing changes verified against a live model (`SMOKE_LIVE=1`),
      results noted below
- [ ] Breaking changes listed in CHANGELOG under **Breaking**
- [ ] No secrets in code, config, tests, or logs
