# Off-cloud flight pusher

OpenSky silently drops TCP connections from GCP/cloud datacenter egress IPs (see the
root `README.md` → **Known limitations**). This pusher runs on a **non-cloud host with a
residential IP** (e.g. a home Docker box), fetches OpenSky, and POSTs the parsed aircraft
to the Cloud Run endpoint `POST /ingest/flight/push`, which writes them to Firestore. The
dashboard then renders flights normally.

```
Home box (residential IP) ──OpenSky fetch──▶ OpenSky API   ✅ not blocked
        │
        └──POST /ingest/flight/push (Bearer token)──▶ Cloud Run ──▶ Firestore ──▶ dashboard
```

## Why this is secure

- **Outbound-only.** The box exposes **no inbound ports** — it only initiates connections
  to OpenSky and Cloud Run. There is nothing to firewall or reverse-proxy.
- **No GCP credentials on the box.** All Firestore/IAM access stays on Cloud Run. The box
  holds only the OpenSky creds and a shared bearer token.
- **Token-guarded write.** The endpoint requires `Authorization: Bearer <INGEST_PUSH_TOKEN>`,
  compared in constant time. The payload is public flight telemetry, so a strong random
  secret over HTTPS is an appropriate guard.

> Tailscale is **not required** for the data path (it's outbound-only). It remains useful
> purely for you to manage the box.

## Setup

1. **Generate a token** and store it in GCP Secret Manager (this is what Cloud Run reads):
   ```bash
   TOKEN=$(openssl rand -hex 32)
   printf "%s" "$TOKEN" | gcloud secrets versions add INGEST_PUSH_TOKEN --data-file=-
   ```
   Then redeploy / update the Cloud Run revision so it picks up the new secret version
   (`./deploy.sh`, or `gcloud run services update telemetry-api --region <REGION>`).

2. **Configure the box:**
   ```bash
   cd deploy/homebox
   cp .env.example .env
   # Edit .env: set PUSH_URL, INGEST_PUSH_TOKEN (= $TOKEN above), and OPENSKY_* creds.
   ```

3. **Run it:**
   ```bash
   docker compose up -d --build
   docker compose logs -f flightpusher
   ```
   Expect log lines like `pushed 33 aircraft to https://.../ingest/flight/push`.

## Verify

```bash
curl -s "https://<service-url>/api/health" | jq '.sources.flight'
# vehicleCount should become > 0 within a poll interval.
```

## Notes

- `RUN_ONCE=true` makes the container fetch+push once and exit — use it with a host cron
  instead of the built-in ticker if you prefer.
- The pusher reuses the server's OpenSky fetch/parse code (`internal/services`), so parsing
  stays consistent with the platform.
- Keep `POLL_INTERVAL_SECONDS` reasonable: OpenSky authenticated accounts allow ~4000
  credits/day.
