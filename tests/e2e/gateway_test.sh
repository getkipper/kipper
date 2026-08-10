#!/usr/bin/env bash
set -uo pipefail

# Gateway integration test: tests the register/lookup/deregister flow.
#
# Usage:
#   GATEWAY_URL=https://kipper.run ./tests/e2e/gateway_test.sh

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

pass=0
fail=0

ok() {
  echo -e "  ${GREEN}✔${NC}  $1"
  pass=$((pass + 1))
}

ko() {
  echo -e "  ${RED}✗${NC}  $1"
  fail=$((fail + 1))
}

echo ""
echo "═══════════════════════════════════════════════"
echo "  Gateway Integration Test"
echo "  URL: $GATEWAY_URL"
echo "═══════════════════════════════════════════════"
echo ""

# Health
echo "Health"
HEALTH=$(curl -s "$GATEWAY_URL/health")
[[ "$HEALTH" == *'"status":"ok"'* ]] && ok "Health returns ok" || ko "Health returns ok"

# Register
echo ""
echo "Register"
REG=$(curl -s -X POST "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d '{"subdomain":"e2e-test-sub","ip":"10.99.99.99"}')
[[ "$REG" == *'"e2e-test-sub"'* ]] && ok "Returns subdomain" || ko "Returns subdomain"
[[ "$REG" == *'"domain"'* ]] && ok "Returns domain" || ko "Returns domain"

TOKEN=$(echo "$REG" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
[[ -n "$TOKEN" ]] && ok "Returns token" || ko "Returns token"

# Duplicate (different IP)
echo ""
echo "Duplicate"
DUP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d '{"subdomain":"e2e-test-sub","ip":"10.0.0.1"}')
[[ "$DUP_CODE" == "409" ]] && ok "Duplicate rejected with 409" || ko "Duplicate rejected (got $DUP_CODE)"

# Same IP renewal
RENEW_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d '{"subdomain":"e2e-test-sub","ip":"10.99.99.99"}')
[[ "$RENEW_CODE" == "201" ]] && ok "Same IP renewal succeeds" || ko "Same IP renewal (got $RENEW_CODE)"

# Ping
echo ""
echo "Ping"
PING=$(curl -s -X POST "$GATEWAY_URL/ping" \
  -H "Content-Type: application/json" \
  -d "{\"token\":\"$TOKEN\"}")
[[ "$PING" == *'"status":"ok"'* ]] && ok "Ping succeeds" || ko "Ping succeeds"

PING_BAD=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/ping" \
  -H "Content-Type: application/json" \
  -d '{"token":"bogus"}')
[[ "$PING_BAD" == "404" ]] && ok "Bad token returns 404" || ko "Bad token (got $PING_BAD)"

# Validation
echo ""
echo "Validation"
BAD_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d '{"subdomain":"UPPER","ip":"203.0.113.1"}')
[[ "$BAD_CODE" == "400" ]] && ok "Uppercase rejected" || ko "Uppercase (got $BAD_CODE)"

NOIP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d '{"subdomain":"valid","ip":""}')
[[ "$NOIP_CODE" == "400" ]] && ok "Empty IP rejected" || ko "Empty IP (got $NOIP_CODE)"

# Deregister
echo ""
echo "Deregister"
DEREG_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d "{\"token\":\"$TOKEN\"}")
[[ "$DEREG_CODE" == "204" ]] && ok "Deregistration succeeds" || ko "Deregistration (got $DEREG_CODE)"

DEREG_BAD=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$GATEWAY_URL/register" \
  -H "Content-Type: application/json" \
  -d '{"token":"bogus"}')
[[ "$DEREG_BAD" == "404" ]] && ok "Bad token deregister returns 404" || ko "Bad token deregister (got $DEREG_BAD)"

# Summary
echo ""
echo "═══════════════════════════════════════════════"
echo -e "  Results: ${GREEN}$pass passed${NC}, ${RED}$fail failed${NC}"
echo "═══════════════════════════════════════════════"
echo ""

[[ "$fail" -eq 0 ]]
