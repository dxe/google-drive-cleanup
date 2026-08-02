#!/usr/bin/env bash
#
# Publish the local review UI on the internet through an ngrok tunnel, gated by
# Google sign-in and restricted to @directactioneverywhere.com accounts.
#
# The tunnel points at the Next dev server (:3000), which proxies /api/* to the
# Go API (:8844). Both stay bound to localhost — ngrok is the only way in, and
# every request through it is authenticated at ngrok's edge before it reaches
# them. See ngrok/traffic-policy.yml for the auth rules.
#
# Rather than trust that the tunnel came up as configured, this script fetches
# its own public URL once it is live and refuses to hand it over unless an
# anonymous request is actually bounced to Google. A tunnel that serves the app
# unauthenticated is torn down. (curl's own User-Agent also suppresses ngrok's
# free-tier interstitial, so this probes the real gate, not the warning page.)
#
# Usage:
#   scripts/share-review.sh    # uses the account's assigned ngrok domain
#
# NGROK_DOMAIN is optional and only useful on a paid plan, where you can reserve
# a domain or bring a custom one. A free account has exactly one auto-assigned
# dev domain (<name>.ngrok-free.app) which it uses by default and cannot change,
# so the URL is already stable between runs — leave NGROK_DOMAIN unset:
#
#   NGROK_DOMAIN=dxe-drive-review.ngrok.app scripts/share-review.sh
#
# Prerequisites, in another two terminals:
#   drive-cleanup review
#   cd web && npm run dev

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
policy="$repo_root/ngrok/traffic-policy.yml"
ui_port="${UI_PORT:-3000}"
api_port="${API_PORT:-8844}"
# ngrok's agent API, which we query for the public URL it was assigned.
agent_api="127.0.0.1:${NGROK_AGENT_PORT:-4040}"
log="$(mktemp -t ngrok-share.XXXXXX.log)"

die() { printf '\033[31merror:\033[0m %s\n' "$1" >&2; exit 1; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$1" >&2; }

[ -f "$policy" ] || die "traffic policy not found at $policy"

command -v ngrok >/dev/null 2>&1 || die \
  "ngrok is not installed. Follow the setup instructions at https://ngrok.com/download to install the agent for this machine."

command -v curl >/dev/null 2>&1 || die "curl is required"

# The authtoken lives in ~/.config/ngrok/ngrok.yml, which is not on a persisted
# volume — expect to redo this after a devcontainer rebuild.
#
# `ngrok config check` only validates the file's syntax: it passes on a config
# with no authtoken at all. So test for the token itself, without printing it.
config_path="$(ngrok config check 2>/dev/null | grep -o '/.*ngrok\.yml' || true)"
if [ -z "${NGROK_AUTHTOKEN:-}" ] &&
   ! { [ -n "$config_path" ] && grep -q '^[[:space:]]*authtoken:[[:space:]]*[^[:space:]]' "$config_path"; }; then
  die "ngrok has no authtoken. Get one from
  https://dashboard.ngrok.com/get-started/your-authtoken then run:

      ngrok config add-authtoken <token>"
fi

# Fail early with a clear message rather than serving a 502 through the tunnel.
if ! curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:$ui_port"; then
  die "nothing is serving http://127.0.0.1:$ui_port. Start the UI first:

      cd web && npm run dev"
fi

if ! curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:$api_port/api/tree"; then
  warn "the Go API on :$api_port is not responding — the UI will load but show
  no data. Start it with:  drive-cleanup review"
fi

args=(http "$ui_port" --traffic-policy-file "$policy"
      --log stdout --log-format logfmt --log-level info)
[ -n "${NGROK_DOMAIN:-}" ] && args+=(--url "https://$NGROK_DOMAIN")

ngrok "${args[@]}" >"$log" 2>&1 &
ngrok_pid=$!

# From here on, any exit path takes the tunnel down with us — an abandoned
# tunnel is an open door, so this must not leak on any path.
cleanup() {
  pkill -P "$ngrok_pid" 2>/dev/null || true
  kill "$ngrok_pid" 2>/dev/null || true
  wait "$ngrok_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

abort() {
  printf '\033[31merror:\033[0m %s\n' "$1" >&2
  echo "--- ngrok log ---" >&2
  cat "$log" >&2
  exit 1
}

# Wait for the agent API to report a public URL.
public_url=""
for _ in $(seq 1 40); do
  kill -0 "$ngrok_pid" 2>/dev/null || abort "ngrok exited before the tunnel came up."
  # Match the public_url field specifically — the same JSON also carries the
  # local forwarding address, which must not be mistaken for the tunnel.
  public_url="$(curl -fsS --max-time 2 "http://$agent_api/api/tunnels" 2>/dev/null \
    | grep -o '"public_url":"https://[^"]*"' | head -1 | cut -d'"' -f4 || true)"
  [ -n "$public_url" ] && break
  sleep 0.5
done
[ -n "$public_url" ] || abort "timed out waiting for ngrok to report a public URL."

# The gate check. An anonymous request must NOT reach the app: expect a redirect
# to Google, or at minimum a non-200. `-o /dev/null` and no `-L` so we see the
# edge's own response rather than following it.
read -r code location < <(curl -sS -o /dev/null --max-time 15 \
  -w '%{http_code} %{redirect_url}\n' "$public_url" || echo "000 ")

if [ "$code" = "200" ]; then
  abort "the tunnel at $public_url served an anonymous request with HTTP 200 —
  the Google sign-in gate is NOT active, so this URL would expose the review UI
  and the Drive database to anyone. Tunnel shut down.

  Check the ngrok log above for a policy error. The likely causes are a
  malformed traffic policy, or the account's traffic-identity quota being used
  up (the free plan covers 3 OAuth users per month:
  https://ngrok.com/docs/pricing-limits/free-plan-limits/)."
fi

case "$location" in
  *accounts.google.com*) gate="Google sign-in (redirects to accounts.google.com)" ;;
  "") gate="non-200 response (HTTP $code) — no anonymous access" ;;
  *) gate="redirect to $location (HTTP $code)" ;;
esac

cat <<EOF

  Review UI published:  $public_url

  Gate verified:        $gate
  Allowed accounts:     @directactioneverywhere.com only
  Sessions:             8h idle / 24h maximum

  Local inspector:      http://$agent_api
  Press Ctrl-C to take the tunnel down.

EOF

wait "$ngrok_pid"
