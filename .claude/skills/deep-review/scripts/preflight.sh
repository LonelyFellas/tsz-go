#!/usr/bin/env bash
# deep-review 阶段 1：机械门禁。一条命令跑完所有确定性检查，
# 产物落在 tmp/deep-review/（已被 gitignore）。
# 退出码非 0 = 至少一项 FAIL。WARN 不阻塞但必须进报告。
set -u

cd "$(git rev-parse --show-toplevel)" || exit 2
ART=tmp/deep-review
SKILL=.claude/skills/deep-review
MIG=internal/platform/database/migrations
TEST_DB_URL="postgres://app:app@localhost:5432/tsz_test?sslmode=disable"
mkdir -p "$ART"

RESULTS=""
FAILS=0
note() { # status name detail
  RESULTS="${RESULTS}$1|$2|$3\n"
  if [ "$1" = "FAIL" ]; then FAILS=$((FAILS + 1)); fi
  echo "[$1] $2 — $3"
}
run() { # name logfile cmd...
  local name=$1 log=$2; shift 2
  if "$@" >"$ART/$log" 2>&1; then
    note PASS "$name" "日志 $ART/$log"
  else
    note FAIL "$name" "日志 $ART/$log"
  fi
}

echo "== deep-review preflight =="
BASE=$(git merge-base main HEAD)
echo "基线: $(git rev-parse --short "$BASE") (merge-base main HEAD)"
echo

# ---- 1. 编译与静态检查 ----
run "go build" build.log go build ./...
run "go vet" vet.log go vet ./...

UNFORMATTED=$(gofmt -l cmd internal docs 2>/dev/null)
if [ -z "$UNFORMATTED" ]; then
  note PASS "gofmt" "全部已格式化"
else
  note FAIL "gofmt" "未格式化: $(echo "$UNFORMATTED" | tr '\n' ' ')"
fi

if command -v staticcheck >/dev/null 2>&1; then
  run "staticcheck" staticcheck.log staticcheck ./...
else
  note SKIP "staticcheck" "未安装（go install honnef.co/go/tools/cmd/staticcheck@latest）"
fi

if command -v govulncheck >/dev/null 2>&1; then
  run "govulncheck" govulncheck.log govulncheck ./...
else
  note SKIP "govulncheck" "未安装（go install golang.org/x/vuln/cmd/govulncheck@latest）"
fi

# ---- 2. 单元测试（不接库） ----
run "单元测试" unit_test.log go test -coverprofile="$ART/unit.out" -coverpkg=./... ./...

# ---- 3. 集成测试（自动拉起 db 容器，-race，连 tsz_test） ----
INTEGRATION_OK=0
if docker info >/dev/null 2>&1; then
  docker compose up -d db >"$ART/db.log" 2>&1
  waited=0
  until docker exec tsz-go-db-1 pg_isready -U app -d tsz >/dev/null 2>&1; do
    sleep 1; waited=$((waited + 1))
    if [ "$waited" -ge 60 ]; then break; fi
  done
  if [ "$waited" -lt 60 ]; then
    docker exec tsz-go-db-1 psql -U app -d postgres -tc \
      "SELECT 1 FROM pg_database WHERE datname='tsz_test'" 2>/dev/null | grep -q 1 \
      || docker exec tsz-go-db-1 psql -U app -d postgres -c "CREATE DATABASE tsz_test" >/dev/null 2>&1
    if DATABASE_URL="$TEST_DB_URL" go test -race -tags=integration \
        -coverprofile="$ART/integration.out" -coverpkg=./... ./... \
        >"$ART/integration_test.log" 2>&1; then
      note PASS "集成测试(-race)" "日志 $ART/integration_test.log"
      INTEGRATION_OK=1
    else
      note FAIL "集成测试(-race)" "日志 $ART/integration_test.log"
    fi
  else
    note FAIL "集成测试(-race)" "db 容器 60s 内未就绪，未执行 — 结论必须降级"
  fi
else
  note FAIL "集成测试(-race)" "Docker 不可用，未执行 — 结论必须降级"
fi

# ---- 4. 迁移编号检查 ----
MIG_ISSUES=""
NUMS=$(ls "$MIG" | grep -oE '^[0-9]+' | sort -n | uniq)
DUPS=$(ls "$MIG" | grep -E '\.up\.sql$' | grep -oE '^[0-9]+' | sort | uniq -d)
[ -n "$DUPS" ] && MIG_ISSUES="编号重复: $(echo "$DUPS" | tr '\n' ' ')"
for n in $NUMS; do
  up=$(ls "$MIG"/${n}_*.up.sql 2>/dev/null | head -1)
  down=$(ls "$MIG"/${n}_*.down.sql 2>/dev/null | head -1)
  if [ -z "$up" ] || [ -z "$down" ]; then
    MIG_ISSUES="$MIG_ISSUES; $n up/down 不成对"
  elif [ ! -s "$down" ]; then
    MIG_ISSUES="$MIG_ISSUES; $n down 为空文件"
  fi
done
if [ -n "$MIG_ISSUES" ]; then
  note FAIL "迁移检查" "$MIG_ISSUES"
else
  note PASS "迁移检查" "编号无重复、up/down 成对且非空"
fi
GAPS=""
prev=""
for n in $NUMS; do
  n10=$((10#$n))
  if [ -n "$prev" ] && [ "$n10" -ne "$((prev + 1))" ]; then GAPS="$GAPS $((prev + 1))..$((n10 - 1))"; fi
  prev=$n10
done
[ -n "$GAPS" ] && note WARN "迁移空号" "缺号:$GAPS — 确认是否为在途分支预留（如 coin=000017），合并顺序要人工确认"

# ---- 5. API 文档三方同步（启发式） ----
CHANGED=$( (git diff --name-only "$BASE"; git ls-files --others --exclude-standard) | sort -u)
if echo "$CHANGED" | grep -q 'internal/platform/httpserver/router.go'; then
  MISSING=""
  echo "$CHANGED" | grep -q 'docs/openapi.yaml' || MISSING="docs/openapi.yaml"
  echo "$CHANGED" | grep -q 'docs/api.md' || MISSING="$MISSING docs/api.md"
  if [ -n "$MISSING" ]; then
    note WARN "API 文档同步" "router.go 变了但未改:$MISSING — 阶段 2 人工核对"
  else
    note PASS "API 文档同步" "router/openapi/api.md 同批变更（一致性仍需阶段 2 人工核对）"
  fi
else
  note PASS "API 文档同步" "router.go 无变更"
fi

# ---- 6. 改动行覆盖率（阈值 80%，cmd/ 豁免） ----
PROFILES="$ART/unit.out"
[ "$INTEGRATION_OK" = 1 ] && [ -f "$ART/integration.out" ] && PROFILES="$PROFILES,$ART/integration.out"
if [ -f "$ART/unit.out" ]; then
  if go run "$SKILL/scripts/diff_coverage.go" -base "$BASE" -profiles "$PROFILES" -threshold 80 \
      >"$ART/diff_coverage.txt" 2>&1; then
    note PASS "改动行覆盖率≥80%" "明细 $ART/diff_coverage.txt"
  else
    note FAIL "改动行覆盖率≥80%" "明细 $ART/diff_coverage.txt"
  fi
  echo; cat "$ART/diff_coverage.txt"
else
  note FAIL "改动行覆盖率≥80%" "单元测试未产出 coverprofile，无法计算"
fi

# ---- 汇总 ----
echo
echo "== 汇总 =="
printf "%b" "$RESULTS" | while IFS='|' read -r st name detail; do
  [ -n "$st" ] && printf "  %-4s %s — %s\n" "$st" "$name" "$detail"
done
echo
if [ "$FAILS" -gt 0 ]; then
  echo "结果: $FAILS 项 FAIL"
  exit 1
fi
echo "结果: 机械门禁全部通过（WARN/SKIP 仍需写入报告）"
