# GCP Telemetry Platform

A real-time telemetry dashboard that aggregates and visualizes live vehicle data for the Austin area:

- **Capital Metro** — live bus positions via the [GTFS-Realtime](https://gtfs.org/realtime/) feed.
- **OpenSky Network** — live aircraft positions over a configurable bounding box. **⚠️ See [Known limitations](#known-limitations): OpenSky blocks GCP egress, so the flight layer does not populate when deployed to Cloud Run.**

The platform doubles as an **SRE demo**: the dashboard exposes live health, SLOs, latency/error metrics, an incident-simulation toolkit, a runbook, and a log viewer.

> This is the **Google Cloud Platform** implementation of the demo, ported from an earlier Azure version. The architecture below reflects the GCP design.

---

## Architecture

```
                        ┌─────────────────────┐
   Cloud Scheduler ────▶│  Cloud Run (Go)     │────▶ Firestore
   (metro + flight      │  - REST API         │      (vehicles collection)
    cron jobs, OIDC)    │  - /ingest workers  │
                        │  - serves React SPA │
                        └─────────┬───────────┘
                                  │ egress via
                                  ▼ VPC connector
                        ┌─────────────────────┐
                        │ Cloud NAT (static IP)│────▶ OpenSky / CapMetro
                        └─────────────────────┘            (upstream APIs)

   Secret Manager ──▶ OPENSKY_CLIENT_ID / OPENSKY_CLIENT_SECRET
```

**Single-container design:** the Go binary serves both the JSON API and the compiled React SPA (`dashboard/dist`). One Cloud Run service hosts everything.

| Concern | GCP service |
|---|---|
| Compute / API / static hosting | Cloud Run (`telemetry-api`) |
| Scheduled ingestion triggers | Cloud Scheduler (`metro-ingester`, `flight-ingester`) |
| Datastore | Firestore (native mode) |
| Secrets | Secret Manager (`OPENSKY_CLIENT_ID`, `OPENSKY_CLIENT_SECRET`) |
| Static egress IP (intended for OpenSky)¹ | VPC connector + Cloud NAT + reserved IP |
| Container images | Artifact Registry |
| Build | Cloud Build |

¹ The static egress IP was intended to get OpenSky calls past datacenter-IP blocking, but testing shows it does **not** — see [Known limitations](#known-limitations).

### Region choice

Resources deploy to **`europe-west3` (Frankfurt)**, set in `deploy.sh`/`destroy.sh`. The original rationale was OpenSky proximity (its API servers are in Switzerland) and EU IP reputation. **This does not actually help** (see below), but the region is otherwise fine and kept for continuity. Note the Terraform `region` variable still defaults to `us-central1`; the scripts override it explicitly.

---

## Known limitations

### OpenSky flight ingestion is blocked from GCP egress (verified 2026-06-18)

OpenSky's API host silently drops TCP connections from the Cloud Run egress IP (`dial tcp ...:443: i/o timeout`), so **the flight layer stays empty when running on GCP**. This was confirmed in `europe-west3` with a dedicated static NAT IP in OAuth2-authenticated mode — region and static IP do **not** bypass it. The same endpoint returns HTTP 200 instantly from a residential IP, and the **Metro feed works fine from the same NAT IP**, so this is OpenSky IP-level blocking of cloud datacenter ranges — not a rate-limit, outage, or misconfiguration.

**Solution (implemented):** an off-cloud **flight pusher** runs on a non-cloud (residential) host, fetches OpenSky from an unblocked IP, and POSTs the parsed aircraft to an authenticated endpoint (`POST /ingest/flight/push`, guarded by `INGEST_PUSH_TOKEN`) that writes them to Firestore. The box is outbound-only and holds **no GCP credentials**. See `deploy/homebox/README.md` to run it, and `dashboard/public/docs/runbook.md` § 3.2 for the incident context.

---

## Project layout

```
.
├── cmd/server/main.go          # Entry point: router, DI, static file server
├── internal/
│   ├── api/                     # HTTP handlers
│   │   ├── api_handlers.go      # Public dashboard API (vehicles, routes, health)
│   │   ├── worker_handlers.go   # /ingest endpoints invoked by Cloud Scheduler
│   │   └── management_handlers.go # Pause/resume Cloud Scheduler jobs
│   ├── services/                # Ingestion + GTFS logic
│   │   ├── flight_ingestion.go  # OpenSky fetch (OAuth2 + circuit breaker)
│   │   ├── metro_ingestion.go   # GTFS-RT protobuf parsing
│   │   └── gtfs_shapes.go       # Static GTFS route shapes
│   └── data/                    # Firestore repository + models
├── dashboard/                   # React + Vite + Leaflet frontend
│   ├── src/
│   ├── public/docs/             # help.md + runbook.md (served at /docs/*)
│   └── dist/                    # Build output baked into the container
├── infra/                       # Terraform (Cloud Run, Scheduler, VPC/NAT, IAM)
├── Dockerfile                   # Multi-stage: build frontend + backend
├── deploy.sh                    # Build image + terraform apply
└── destroy.sh                   # terraform destroy (demo-at-rest teardown)
```

---

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| GET | `/health`, `/api/health` | Liveness / aggregate source health |
| GET | `/api/vehicles/current?source={metro\|flight}` | Active vehicles (5-min window) |
| GET | `/api/vehicles/history?id={vehicleId}` | Vehicle path (12-hour window) |
| GET | `/api/routes/` | GTFS route shapes |
| GET | `/api/routes/stops/all` | Bus stops |
| GET | `/api/manage/status` | Scheduler job status |
| GET | `/api/manage/opensky-status` | OpenSky upstream/rate-limit health |
| POST | `/api/manage/start` \| `/api/manage/stop` | Resume / pause Cloud Scheduler jobs |
| POST | `/ingest/metro` \| `/ingest/flight` | Worker triggers (Cloud Scheduler, OIDC) |

---

## Local development

### Prerequisites

- Go 1.24+
- Node.js 20+
- (Optional) `gcloud` CLI + Application Default Credentials for live Firestore

### Backend

```bash
# Dev mode tolerates a nil Firestore client and skips auth on /ingest + /manage
ENV=dev go run ./cmd/server
# Server listens on :8080 (override with PORT)
```

Relevant environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `ENV` | _(unset)_ | `dev` relaxes auth and tolerates missing GCP credentials |
| `GOOGLE_CLOUD_PROJECT` | `gcp-telemetry-platform` | Firestore project |
| `FIRESTORE_DB_ID` | `(default)` | Firestore database ID |
| `GCP_LOCATION` | `us-central1` | Region for Cloud Scheduler job names |
| `OPENSKY_BBOX` | `29.8,-98.2,30.8,-97.2` | `lamin,lomin,lamax,lomax` |
| `OPENSKY_CLIENT_ID` / `OPENSKY_CLIENT_SECRET` | _(unset)_ | OpenSky OAuth2 (4000 credits/day); anonymous otherwise |
| `METRO_GTFS_RT_URL` | CapMetro feed | GTFS-RT source |
| `MANAGEMENT_ADMIN_TOKEN` | _(unset)_ | Bearer token guarding `/api/manage/start\|stop` |

### Frontend

```bash
cd dashboard
npm install
npm run dev      # Vite dev server; proxies API to VITE_API_BASE_URL (.env.development)
npm run build    # Outputs to dashboard/dist (served by the Go binary in prod)
```

---

## Deployment

`deploy.sh` is idempotent: it enables APIs, ensures the Artifact Registry repo and placeholder secrets exist, builds the container with Cloud Build, and applies Terraform.

```bash
./deploy.sh
```

Set real OpenSky credentials before/after the first deploy:

```bash
printf 'YOUR_CLIENT_ID'     | gcloud secrets versions add OPENSKY_CLIENT_ID     --data-file=-
printf 'YOUR_CLIENT_SECRET' | gcloud secrets versions add OPENSKY_CLIENT_SECRET --data-file=-
```

> **Note:** `deploy.sh`/`destroy.sh` set `REGION="europe-west3"`, while the Terraform `region` variable defaults to `us-central1`. The scripts pass `region` explicitly, so they win — keep these in sync if you change regions.

### Teardown (demo-at-rest)

```bash
./destroy.sh   # destroys Cloud Run/Scheduler/VPC; preserves Firestore data + secrets
```

Idle cost after teardown is effectively zero. Re-run `./deploy.sh` to bring the demo back online.

---

## Operations

- **User guide:** `dashboard/public/docs/help.md` (in-app `📖`/`❓`).
- **Incident runbook:** `dashboard/public/docs/runbook.md` (SRE procedures, alerts, escalation).

Both are served at `/docs/*.md` from the built SPA.
