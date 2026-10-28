#!/usr/bin/env bash
# Drives the stack running in Kubernetes over HTTP. Every code carries a run id, so a kept Postgres volume never collides.
# Needs: curl, python3. The stack must be up: make cluster-up
set -euo pipefail

API="${API:-http://127.0.0.1:5237}"
RABBIT="${RABBIT:-http://127.0.0.1:15727}"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'

say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }

ADMIN_EMAIL="${ADMIN_EMAIL:-admin@freight.local}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-FreightAdmin2026}"
CUSTOMER_PASSWORD="Smoke-Passw0rd1"
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }

# Section 0 only needs "a small sailing" and "one booking"; a system with another shape adapts these two lines.
open_small() { open_sailing "$1" 5; }
book_one() { book "$1" "$2" 1; }

open_sailing() { # code capacity -> prints nothing
  curl -sf -X POST "$API/api/sailings" -H "$JSON" -H "$ADM" \
    -d "{\"code\":\"$1\",\"vessel\":\"MV Smoke\",\"origin\":\"Santos\",\"destination\":\"Rotterdam\",\"capacityTeu\":$2}" >/dev/null \
    || fail "could not open sailing $1"
}
book() { # code reference teu -> prints booking id
  curl -sf -X POST "$API/api/bookings" -H "$JSON" -H "$CUS" \
    -d "{\"sailingCode\":\"$1\",\"reference\":\"$2\",\"teu\":$3}" | jget "['id']"
}
booking_field() { curl -sf -H "$ADM" "$API/api/bookings/$1" | jget "['$2']"; }
remaining() { curl -sf -H "$ADM" "$API/api/sailings/$1" | jget "['remainingTeu']"; }

wait_for() { # description command... : polls up to 30s until the command succeeds
  local what="$1"; shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  fail "timed out waiting for $what"
}
status_is() { [ "$(booking_field "$1" status)" = "$2" ]; }
remaining_is() { [ "$(remaining "$1")" = "$2" ]; }
terminal() { case "$(booking_field "$1" status)" in Issued|Rejected|Reversed) return 0 ;; *) return 1 ;; esac; }
event_types() { curl -sf -H "$ADM" "$API/api/bookings/$1/events" | python3 -c "import sys,json; print(' '.join(e['type'] for e in json.load(sys.stdin)))"; }
dlq_is() { [ "$(dlq_count)" = "$1" ]; }
dlq_count() { curl -sf -u platform:platform "$RABBIT/api/queues/%2F/documents.work.dlq" | jget "['messages']"; }

curl -sf "$API/api/health" >/dev/null || fail "API is not healthy at $API; run: make cluster-up"

echo "0. authentication and ownership"
[ "$(code_of "$API/api/sailings")" = 401 ] || fail "an anonymous request should be 401"
[ "$(code_of -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"wrong-Passw0rd\"}")" = 401 ] || fail "a wrong password should be 401"
ADM="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | token_of)" || fail "the administrator could not sign in"
CUS="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"smoke-$RUN@example.com\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)" || fail "could not register a customer"
OTHER="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"other-$RUN@example.com\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)"
[ "$(code_of -X POST "$API/api/sailings" -H "$JSON" -H "$CUS" -d '{"code":"X"}')" = 403 ] || fail "a customer must not open sailings"
open_small "A-$RUN"
IDA="$(book_one "A-$RUN" "BK-$RUN-OWN")"
[ "$(code_of -H "$OTHER" "$API/api/bookings/$IDA")" = 404 ] || fail "another customer must get 404 for someone else's booking"
[ "$(code_of -H "$CUS" "$API/api/bookings/$IDA/events")" = 403 ] || fail "the event log is staff only"
[ "$(code_of -X POST -H "$OTHER" "$API/api/bookings/$IDA/reversal")" = 404 ] || fail "another customer must not reverse it"
say "401 anonymous, 401 bad password, 403 customer opening a sailing, 404 for another customer, 403 on the event log"

echo "0b. password reset ends the old sessions (needs the dev mailbox the demo stack turns on)"
RESET_EMAIL="reset-$RUN@example.com"
OLD="$(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)"
[ "$(code_of -H "Authorization: Bearer $OLD" "$API/api/auth/me")" = 200 ] || fail "the new session should work"
[ "$(code_of -X POST "$API/api/auth/forgot-password" -H "$JSON" -d "{\"email\":\"nobody-$RUN@example.com\"}")" = 202 ] || fail "an unknown address must look the same"
curl -sf -X POST "$API/api/auth/forgot-password" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\"}" >/dev/null || fail "forgot-password failed"
LINK="$(curl -sf "$API/api/dev/mailbox?to=$RESET_EMAIL" | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['link'])")"
link_param() { python3 -c "import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).query)['$1'][0])" "$LINK"; }
[ "$(code_of -X POST "$API/api/auth/reset-password" -H "$JSON" -d "{\"userId\":\"$(link_param reset)\",\"token\":\"$(link_param token)\",\"newPassword\":\"Another-Passw0rd9\"}")" = 204 ] || fail "reset failed"
sleep 3 # the session version is cached for a moment
[ "$(code_of -H "Authorization: Bearer $OLD" "$API/api/auth/me")" = 401 ] || fail "the old access token should be revoked"
[ "$(code_of -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\",\"password\":\"Another-Passw0rd9\"}")" = 200 ] || fail "the new password should work"
say "reset link works once, the old access token is revoked, the new password signs in"

echo "1. input is validated at the door"
open_sailing "V-$RUN" 10
for body in \
  "{\"sailingCode\":\"V-$RUN\",\"reference\":\"\",\"teu\":1}" \
  "{\"sailingCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"teu\":0}" \
  "{\"sailingCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"teu\":21}" \
  "{\"sailingCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"teu\":4294967297}" \
  "{\"sailingCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"teu\":9223372036854775807}"; do
  code="$(code_of -X POST "$API/api/bookings" -H "$JSON" -H "$CUS" -d "$body")"
  [ "$code" = 400 ] || fail "expected 400 for $body, got $code"
done
say "empty, zero, over-limit and overflowing TEU are all 400"

echo "2. happy path"
open_sailing "H-$RUN" 10
ID="$(book "H-$RUN" "BK-$RUN-1" 3)"
wait_for "booking to be Issued" status_is "$ID" Issued
[ "$(booking_field "$ID" feeCents)" = 277500 ] || fail "fee should be 3 x 90000 + 7500 cents"
[ "$(curl -sf -H "$ADM" "$API/api/documents/$ID" | jget "['status']")" = Issued ] || fail "bill of lading not issued"
wait_for "remaining TEU to be 7" remaining_is "H-$RUN" 7
TYPES="$(event_types "$ID")"
for t in capacity.reserve capacity.held document.issued; do
  case "$TYPES" in *"$t"*) ;; *) fail "Kafka log is missing $t (has: $TYPES)" ;; esac
done
say "issued, fee 277500, remaining 7, Kafka log has the three steps"

echo "3. last unit: three bookings, one commits, two are short and write no hold"
open_sailing "L-$RUN" 1
TMP="$(mktemp -d)"
for n in A B C; do ( book "L-$RUN" "BK-$RUN-$n" 1 >"$TMP/$n" ) & done
wait
IDS=""
for n in A B C; do [ -s "$TMP/$n" ] || fail "booking $n was not accepted"; IDS="$IDS $(cat "$TMP/$n")"; done
rm -rf "$TMP"
for i in $IDS; do wait_for "booking $i to settle" terminal "$i"; done
ISSUED=0; SHORT=0
for i in $IDS; do
  s="$(booking_field "$i" status)"
  if [ "$s" = Issued ]; then ISSUED=$((ISSUED + 1)); fi
  if [ "$s" = Rejected ]; then
    case "$(booking_field "$i" reason)" in Short*) SHORT=$((SHORT + 1)); case "$(event_types "$i")" in *capacity.held*) fail "the short loser has a hold" ;; esac ;; esac
  fi
done
[ "$ISSUED" = 1 ] && [ "$SHORT" = 2 ] || fail "expected 1 issued and 2 short, got $ISSUED issued and $SHORT short"
[ "$(remaining "L-$RUN")" = 0 ] || fail "remaining should be 0"
say "1 issued, 2 short, remaining 0, no hold for the losers"

echo "4. poison: committed as rejected, dead-lettered, the TEU comes back"
open_sailing "P-$RUN" 5
BEFORE="$(dlq_count)"
IDP="$(book "P-$RUN" "BK-POISON-$RUN" 2)"
wait_for "poison booking to be Rejected" status_is "$IDP" Rejected
wait_for "TEU to be restored to 5" remaining_is "P-$RUN" 5
wait_for "documents.work.dlq to gain one message" dlq_is $((BEFORE + 1))
[ "$(curl -sf -H "$ADM" "$API/api/documents/$IDP" | jget "['status']")" = Rejected ] || fail "rejection should be committed on the document row"
[ "$(booking_field "$IDP" capacityReleasedAt)" != None ] || fail "booking should record the release"
say "rejected, dead-lettered (+1 in documents.work.dlq), remaining back to 5"

echo "4b. dead letters can be listed by staff only, and replayed"
[ "$(code_of -H "$CUS" "$API/api/admin/dead-letters")" = 403 ] || fail "dead letters are staff only"
curl -sf -H "$ADM" "$API/api/admin/dead-letters" | python3 -c "import sys,json; q={x['queue']:x for x in json.load(sys.stdin)}; assert q['documents.work']['count']>=1, q" || fail "the dead-letter queue should list the poison message"
BEFORE_REPLAY="$(dlq_count)"
REPLAYED="$(curl -sf -X POST -H "$ADM" "$API/api/admin/dead-letters/documents.work/replay?max=100" | jget "['replayed']")" || fail "replay failed"
[ "$REPLAYED" -ge 1 ] || fail "replay should have taken at least one message"
# The poison reference still fails for the same reason, so it is retried and dead-lettered again: replay is for fixed causes.
wait_for "the replayed poison message to be dead-lettered again" dlq_is "$BEFORE_REPLAY"
say "listed, replayed $REPLAYED, dead-lettered again because the cause is still there"

echo "5. reversal races the final mark: the reversal wins, the issued document stays"
open_sailing "R-$RUN" 6
WON=0
for attempt in 1 2 3; do
  IDR="$(book "R-$RUN" "BK-$RUN-R$attempt" 2)"
  # The bill of lading exists before the booking is marked Issued (the final mark is delayed in the demo cluster).
  wait_for "bill of lading to exist" curl -sf -H "$ADM" "$API/api/documents/$IDR"
  if status_is "$IDR" Held; then WON=1; break; fi
  say "window missed on attempt $attempt (booking already Issued); the reversal still has to hold; retrying"
  curl -sf -X POST -H "$CUS" "$API/api/bookings/$IDR/reversal" >/dev/null || true
  wait_for "booking to be Reversed" status_is "$IDR" Reversed
done
[ "$WON" = 1 ] || fail "never caught the booking between document issued and final mark"
curl -sf -X POST -H "$CUS" "$API/api/bookings/$IDR/reversal" >/dev/null || fail "reversal was refused"
wait_for "booking to be Reversed" status_is "$IDR" Reversed
sleep 3 # longer than the final-mark delay: the late mark must not undo the reversal
status_is "$IDR" Reversed || fail "the final mark overwrote the reversal"
[ "$(curl -sf -H "$ADM" "$API/api/documents/$IDR" | jget "['status']")" = Issued ] || fail "the issued document was rolled back"
[ "$(booking_field "$IDR" billOfLadingNumber)" = None ] || fail "the late final mark should have been skipped"
wait_for "TEU to come back" remaining_is "R-$RUN" 6
code="$(code_of -X POST -H "$CUS" "$API/api/bookings/$IDR/reversal")"
[ "$code" = 409 ] || fail "second reversal should be 409, got $code"
say "booking Reversed, document still Issued, remaining 6, second reversal 409"

echo "golang-freight smoke passed"
