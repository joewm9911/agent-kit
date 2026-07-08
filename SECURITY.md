# Security Policy

## Supported versions

Pre-1.0: only the latest tagged release and `main` receive fixes.

## Reporting a vulnerability

Please use **GitHub private vulnerability reporting** (Security →
Report a vulnerability) on this repository. Do not open public issues
for security problems. You should receive a response within 7 days.

## Scope notes for deployers

- **Secrets**: the framework reads credentials via the `secrets` provider
  (environment / file); never commit keys into configuration files. Example
  runners read keys from the OS keychain or environment only.
- **Script execution**: `exec` capabilities should run inside a sandbox.
  Set `exec.default_sandbox` and `require_sandbox: true` in production so
  an exec tool without a sandbox fails at assembly instead of running on
  the host.
- **Webhooks**: IM channel webhooks verify platform signatures; keep the
  verification token in the secrets provider.
- **Approval gates**: mutating tools can be gated behind human approval
  (`approval` rules); combined with durable suspend, approvals survive
  restarts — prefer this over disabling approval for long-running flows.
