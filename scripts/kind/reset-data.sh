#!/usr/bin/env bash
# Reset database directly via kubectl (doesn't rely on backend)
set -euo pipefail

echo "=== Resetting database ==="
kubectl exec -n mainloop postgres-0 -- psql -U mainloop -d mainloop -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
echo "=== Reset complete ==="
