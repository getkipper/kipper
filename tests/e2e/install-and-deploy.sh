#!/usr/bin/env bash
set -euo pipefail

# End-to-end test: install a cluster and deploy an app.
#
# Usage:
#   ./tests/e2e/install-and-deploy.sh <server-ip> <ssh-key-path>
#
# Prerequisites:
#   - A fresh Linux server (Ubuntu 22.04/24.04 or Debian 11/12)
#   - SSH key access as root
#   - The kip binary built: cd kip && go build -o kip .
#
# This script is meant to be run manually before releases.
# It tests the full user journey from zero to running app.

HOST="${1:?Usage: $0 <server-ip> <ssh-key-path>}"
SSH_KEY="${2:?Usage: $0 <server-ip> <ssh-key-path>}"
KIP="./kip/kip"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

pass=0
fail=0

check() {
  local desc="$1"
  shift
  if eval "$*" > /dev/null 2>&1; then
    echo -e "  ${GREEN}✔${NC}  $desc"
    ((pass++))
  else
    echo -e "  ${RED}✗${NC}  $desc"
    ((fail++))
  fi
}

echo ""
echo "═══════════════════════════════════════════════"
echo "  Kipper E2E Test"
echo "  Host: $HOST"
echo "═══════════════════════════════════════════════"
echo ""

# Step 1: Verify SSH access
echo "Step 1: SSH access"
check "Can SSH into server" ssh -o IdentitiesOnly=yes -i "$SSH_KEY" -o ConnectTimeout=10 "root@$HOST" "hostname"

# Step 2: Build kip
echo ""
echo "Step 2: Build CLI"
check "kip binary builds" bash -c "cd kip && go build -o kip ."

# Step 3: Install cluster
echo ""
echo "Step 3: Install cluster"
INSTALL_OUTPUT=$($KIP install --host "$HOST" --ssh-key "$SSH_KEY" --admin-email "e2e@kipper.test" 2>&1) || true
echo "$INSTALL_OUTPUT"

check "Install completed" echo "$INSTALL_OUTPUT" | grep -q "Cluster ready"
check "Kubeconfig created" test -f "$HOME/.kip/clusters/"*.yaml
check "Config updated" test -f "$HOME/.kip/config.yaml"

# Step 4: Verify status
echo ""
echo "Step 4: Cluster status"
STATUS_OUTPUT=$($KIP status 2>&1) || true
echo "$STATUS_OUTPUT"

check "Status shows healthy" echo "$STATUS_OUTPUT" | grep -q "k3s"
check "Node is Ready" echo "$STATUS_OUTPUT" | grep -q "Ready"
check "Traefik running" echo "$STATUS_OUTPUT" | grep -q "Traefik"
check "cert-manager running" echo "$STATUS_OUTPUT" | grep -q "cert-manager"
check "Longhorn running" echo "$STATUS_OUTPUT" | grep -q "Longhorn"
check "Dex running" echo "$STATUS_OUTPUT" | grep -q "Dex"

# Step 5: Deploy an app
echo ""
echo "Step 5: Deploy test app"
DEPLOY_OUTPUT=$($KIP app deploy --name e2e-test --image nginx:latest --port 80 --project default 2>&1) || true
echo "$DEPLOY_OUTPUT"

check "Deploy succeeded" echo "$DEPLOY_OUTPUT" | grep -q "Deployment created"
check "Service created" echo "$DEPLOY_OUTPUT" | grep -q "Service created"
check "Ingress created" echo "$DEPLOY_OUTPUT" | grep -q "Ingress created"

# Step 6: Verify app is running
echo ""
echo "Step 6: Verify app"
sleep 10

LIST_OUTPUT=$($KIP app list --project default 2>&1) || true
echo "$LIST_OUTPUT"

check "App appears in list" echo "$LIST_OUTPUT" | grep -q "e2e-test"
check "App is running" echo "$LIST_OUTPUT" | grep -q "running"

# Step 7: Test env and secrets
echo ""
echo "Step 7: Environment and secrets"
$KIP app env set e2e-test TEST_VAR=hello > /dev/null 2>&1 || true
ENV_OUTPUT=$($KIP app env list e2e-test 2>&1) || true
check "Env var set and visible" echo "$ENV_OUTPUT" | grep -q "TEST_VAR"

$KIP app secret set e2e-test SECRET_KEY=mysecret > /dev/null 2>&1 || true
SECRET_OUTPUT=$($KIP app secret list e2e-test 2>&1) || true
check "Secret set and listed" echo "$SECRET_OUTPUT" | grep -q "SECRET_KEY"

REVEAL_OUTPUT=$($KIP app secret reveal e2e-test SECRET_KEY 2>&1) || true
check "Secret reveal works" echo "$REVEAL_OUTPUT" | grep -q "mysecret"

# Step 8: Scale
echo ""
echo "Step 8: Scale"
$KIP app scale e2e-test --replicas 2 > /dev/null 2>&1 || true
sleep 15
SCALE_OUTPUT=$($KIP app list --project default 2>&1) || true
check "Scaled to 2 replicas" echo "$SCALE_OUTPUT" | grep "e2e-test" | grep -q "/2"

# Step 9: Restart
echo ""
echo "Step 9: Restart"
RESTART_OUTPUT=$($KIP app restart e2e-test 2>&1) || true
check "Restart succeeded" echo "$RESTART_OUTPUT" | grep -q "Restart triggered"

# Step 10: Password reset
echo ""
echo "Step 10: Auth"
RESET_OUTPUT=$($KIP auth reset-password 2>&1) || true
check "Password reset works" echo "$RESET_OUTPUT" | grep -q "Admin password reset"

# Step 11: Clean up
echo ""
echo "Step 11: Clean up"
$KIP app delete e2e-test > /dev/null 2>&1 || true
DELETE_LIST=$($KIP app list --project default 2>&1) || true
check "App deleted" ! echo "$DELETE_LIST" | grep -q "e2e-test"

# Summary
echo ""
echo "═══════════════════════════════════════════════"
echo -e "  Results: ${GREEN}$pass passed${NC}, ${RED}$fail failed${NC}"
echo "═══════════════════════════════════════════════"
echo ""

if [ "$fail" -gt 0 ]; then
  exit 1
fi
