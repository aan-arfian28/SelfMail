#!/usr/bin/env bash
# One-time setup of a fresh Stalwart (>= 1.x) via its JMAP Bootstrap object.
#   STALWART_URL=http://localhost:8081 STALWART_RECOVERY_ADMIN=admin:secret \
#   MAIL_HOST=mail.example.com DEFAULT_DOMAIN=example.com TLS=true \
#   ./scripts/bootstrap-stalwart.sh
# Prints the permanent admin credentials (use them as STALWART_USER / STALWART_PASSWORD).
# Afterwards restart Stalwart once: docker compose restart stalwart
set -euo pipefail
: "${STALWART_URL:?}" "${STALWART_RECOVERY_ADMIN:?user:password}" "${MAIL_HOST:?}" "${DEFAULT_DOMAIN:?}"
TLS="${TLS:-false}"   # true = request a Let's Encrypt certificate (needs public DNS + port 80/443)
resp=$(curl -fsS -u "$STALWART_RECOVERY_ADMIN" "$STALWART_URL/jmap/" -H 'Content-Type: application/json' -d "{
  \"using\":[\"urn:ietf:params:jmap:core\",\"urn:stalwart:jmap\"],
  \"methodCalls\":[[\"x:Bootstrap/set\",{\"update\":{\"singleton\":{
    \"defaultHostname\":\"$MAIL_HOST\",\"defaultDomain\":\"$DEFAULT_DOMAIN\",
    \"requestTlsCertificate\":$TLS,\"generateDkimKeys\":true}}},\"c1\"]]}")
echo "$resp" | python3 -c '
import json,sys
r=json.load(sys.stdin)["methodResponses"][0]
if r[0]=="error": sys.exit("bootstrap failed: %s" % r[1])
u=r[1]["updated"]["singleton"]
print("STALWART_USER=%s\nSTALWART_PASSWORD=%s" % (u["username"],u["secret"]))'
echo "Now restart Stalwart so the configuration takes effect." >&2
