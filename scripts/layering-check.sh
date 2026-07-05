#!/usr/bin/env bash
# 分层依赖守卫:验证 import 方向符合 docs/architecture-restructure-plan.md §7。
#   core → 无内部依赖;protocol → core;runtime → core+protocol;
#   L3(agent/skill/todo/askuser)不向上;L1-L4 永不 import impl/。
# 用法:./scripts/layering-check.sh(仓库根目录);CI 里同样一行。
set -euo pipefail
cd "$(dirname "$0")/.."
MOD="github.com/joewm9911/agent-kit"
fail=0

deps() { go list -deps "$1" 2>/dev/null | grep "^$MOD/" | grep -v "^$1$" || true; }

check() { # check <pattern-desc> <pkgs> <forbidden-regex>
  local desc="$1" pkgs="$2" forbid="$3" hit
  hit=$(go list -deps $pkgs 2>/dev/null | grep -E "$forbid" | sort -u || true)
  if [ -n "$hit" ]; then
    echo "✗ $desc 违规依赖:"; echo "$hit" | sed 's/^/    /'; fail=1
  else
    echo "✓ $desc"
  fi
}

check "core 零内部依赖"        "./core/..."     "$MOD/(protocol|runtime|agent|skill|todo|askuser|serving|config|std|impl)(/|$)"
check "protocol 只依赖 core"   "./protocol/..." "$MOD/(runtime|agent|skill|todo|askuser|serving|config|std|impl)(/|$)"
check "runtime 不向上依赖"     "./runtime/..."  "$MOD/(agent|skill|todo|askuser|serving|config|std|impl)(/|$)"
check "L3 不向上依赖"          "./agent/... ./skill/... ./todo/... ./askuser/..." "$MOD/(serving|config|std|impl)(/|$)"
check "serving 不依赖装配层"   "./serving/..."  "$MOD/(config|std|impl)(/|$)"
check "L1-L4 不依赖 impl"      "./core/... ./protocol/... ./runtime/... ./agent/... ./skill/... ./todo/... ./askuser/... ./serving/..." "$MOD/impl/"

[ $fail -eq 0 ] && echo "layering OK" || exit 1
