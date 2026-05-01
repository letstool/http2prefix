#!/bin/bash
# Smoke tests for http2prefix — requires a running server on 127.0.0.1:8080
set -e
BASE="http://127.0.0.1:8080"
PASS=0; FAIL=0

check() {
  local desc="$1" actual="$2" expected="$3"
  if echo "$actual" | grep -q "$expected"; then
    echo "  PASS: $desc"
    ((PASS++))
  else
    echo "  FAIL: $desc"
    echo "        expected: $expected"
    echo "        actual:   $actual"
    ((FAIL++))
  fi
}

echo "=== http2prefix smoke tests ==="

echo ""
echo "-- GET / --"
R=$(curl -sf "$BASE/")
check "GET / returns HTML" "$R" "http2prefix"

echo ""
echo "-- GET /openapi.json --"
R=$(curl -sf "$BASE/openapi.json")
check "GET /openapi.json returns JSON" "$R" "http2prefix"

echo ""
echo "-- POST /api/v1/asprefix (IPv4) --"
R=$(curl -sf -X POST "$BASE/api/v1/asprefix" -H "Content-Type: application/json" -d '{"query":"1.1.1.1"}')
check "IP lookup query_type=ip"    "$R" '"query_type":"ip"'
check "IP lookup status not ERROR" "$R" '"status":"'

echo ""
echo "-- POST /api/v1/asprefix (IPv6) --"
R=$(curl -sf -X POST "$BASE/api/v1/asprefix" -H "Content-Type: application/json" -d '{"query":"2606:4700::"}')
check "IPv6 lookup query_type=ip"  "$R" '"query_type":"ip"'

echo ""
echo "-- POST /api/v1/asprefix (AS uppercase) --"
R=$(curl -sf -X POST "$BASE/api/v1/asprefix" -H "Content-Type: application/json" -d '{"query":"AS13335"}')
check "AS lookup query_type=asnum" "$R" '"query_type":"asnum"'

echo ""
echo "-- POST /api/v1/asprefix (AS lowercase) --"
R=$(curl -sf -X POST "$BASE/api/v1/asprefix" -H "Content-Type: application/json" -d '{"query":"as13335"}')
check "AS lowercase query_type"    "$R" '"query_type":"asnum"'

echo ""
echo "-- POST /api/v1/asprefix (invalid) --"
R=$(curl -sf -X POST "$BASE/api/v1/asprefix" -H "Content-Type: application/json" -d '{"query":"notanip"}')
check "Invalid returns ERROR"       "$R" '"status":"ERROR"'
check "Invalid query_type=unknown" "$R" '"query_type":"unknown"'

echo ""
echo "-- GET /db/prefix2as --"
CODE=$(curl -sf -o /dev/null -w "%{http_code}" "$BASE/db/prefix2as" || echo "000")
check "GET /db/prefix2as reachable" "$CODE" "200\|404"

echo ""
echo "-- GET /db/as2prefix --"
CODE=$(curl -sf -o /dev/null -w "%{http_code}" "$BASE/db/as2prefix" || echo "000")
check "GET /db/as2prefix reachable" "$CODE" "200\|404"

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
[ "$FAIL" -eq 0 ]
