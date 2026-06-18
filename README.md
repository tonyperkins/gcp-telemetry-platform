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
   (metro cron, OIDC)   │  - REST API         │      (vehicles collection,
                        │  - /ingest workers  │       24h TTL on docs)
                        │  - serves React SPA │
                        └─────────┬───────────┘
                                  │ default direct
                                  ▼ internet egress
                            CapMetro GTFS-RT
                          (OpenSky upstream)

   Off-cloud flight pusher ──▶ POST /ingest/flight/push  (Bearer token)
   (residential IP)

   Secret Manager ──▶ OPENSKY_CLIENT_ID / OPENSKY_CLIENT_SECRET / INGEST_PUSH_TOKEN
```

**Single-container design:** the Go binary serves both the JSON API and the compiled React SPA (`dashboard/dist`). One Cloud Run service hosts everything.

| Concern | GCP service |
|---|---|
| Compute / API / static hosting | Cloud Run (`telemetry-api`, scales to zero) |
| Scheduled ingestion trigger | Cloud Scheduler (`metro-ingester`) |
| Datastore | Firestore (native mode, 24h TTL on `vehicles`) |
| Secrets | Secret Manager (`OPENSKY_CLIENT_ID`, `OPENSKY_CLIENT_SECRET`, `INGEST_PUSH_TOKEN`) |
| Container images | Artifact Registry |
| Build | Cloud Build |

**No VPC / Cloud NAT.** An earlier design ran a Serverless VPC Access connector + Cloud NAT + reserved static IP to give OpenSky a stable egress IP. OpenSky blocks GCP datacenter IPs regardless of region or static IP (see [Known limitations](#known-limitations)), so that stack cost ~$45/mo for zero benefit and has been removed. Flights now arrive via the off-cloud pusher, and the Metro feed is reachable over Cloud Run's default direct internet egress.

### Region choice

Resources deploy to **`europe-west3` (Frankfurt)**, set in `deploy.sh`/`destroy.sh`. The original rationale was OpenSky proximity (its API servers are in Switzerland) and EU IP reputation. **This does not actually help** (see below), but the region is otherwise fine and kept for continuity. Note the Terraform `region` variable still defaults to `us-central1`; the scripts override it explicitly.

### Cost

Designed to cost **a few dollars a month or less**:

- **Cloud Run** scales to zero (`min_instance_count = 0`, `cpu_idle = true`) and is capped at 2 instances, so there is no idle compute charge and no runaway-bill risk.
- **Metro polling** runs every 2 minutes (`metro_polling_cron`), keeping Cloud Run invocations and Firestore writes inside the free tier.
- **Firestore** documents carry a 24h TTL (`expire_at` field), so the append-only `vehicles` collection self-prunes and storage stays in the free tier.
- **No always-on networking** (VPC connector / Cloud NAT / static IP removed) — this was the single largest line item.

| State | Approx. monthly cost |
|---|---|
| Deployed and running | ~$0–2 (largely free tier) |
| Torn down via `destroy.sh` | < $0.10 (Firestore data + secrets at rest) |

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
├── .github/workflows/           # CI, terraform-plan (PR), deploy (manual)
├── infra/                       # Terraform (Cloud Run, Scheduler, Firestore TTL, IAM)
│   ├── backend.tf               # Remote state (GCS)
│   └── bootstrap-cicd.sh        # One-time WIF + service-account setup
├── Dockerfile                   # Multi-stage: build frontend + backend
├── deploy.sh                    # Local/break-glass: build image + terraform apply
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

The primary path is **CI/CD via GitHub Actions** (below). `deploy.sh` remains as a local/break-glass alternative.

### CI/CD (GitHub Actions)

| Workflow | Trigger | Auth | Does |
|---|---|---|---|
| `ci.yml` | every PR + push to `main` | none | `go build`/`vet`/`test`, dashboard build, `terraform fmt`/`validate` |
| `terraform-plan.yml` | PRs touching `infra/**` | WIF → `gh-planner` (read-only) | `terraform plan`, posted as a PR comment |
| `deploy.yml` | manual (`workflow_dispatch`) | WIF → `gh-deployer` | Cloud Build image + `terraform apply` |

**Keyless auth via Workload Identity Federation.** No service-account JSON keys exist; GitHub's OIDC token is exchanged for short-lived GCP credentials. The WIF provider is locked to this repo (`attribute.repository`), and the two identities are least-privilege:

- **`gh-planner`** — `roles/viewer` only; impersonable from any ref, so PR plans can run safely.
- **`gh-deployer`** — curated, non-escalating roles (Cloud Run / Scheduler / Artifact Registry / Cloud Build / Firestore + state-bucket access, plus `actAs` on the runtime SAs). Deliberately *lacks* project-IAM-admin, SA-admin and secret-admin, so CI can deploy images but cannot modify IAM, service accounts or secrets — those stay a human-run `deploy.sh` operation. Impersonation is restricted to `refs/heads/main`.

**Deploys are gated.** `deploy.yml` only runs from the manual *Run workflow* button, and the `production` GitHub Environment requires a reviewer's approval before the job executes — a merge never spins up paid resources on its own.

One-time setup of the WIF pool, providers and service accounts is scripted in **`infra/bootstrap-cicd.sh`** (idempotent; run once with an admin account). Repo variables (`WIF_PROVIDER`, `GCP_*`) wire the workflows to the project.

### Local / break-glass deploy

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

### Remote state

Terraform state lives in a versioned, private GCS bucket (`gcp-telemetry-platform-tfstate`, see `infra/backend.tf`) with native state locking — never in git. State is never committed (`*.tfstate` is gitignored); `.terraform.lock.hcl` *is* committed to pin provider versions.

The bucket is created out-of-band, since it can't be managed by the state it holds:

```bash
gcloud storage buckets create gs://gcp-telemetry-platform-tfstate \
  --location=europe-west3 --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets update gs://gcp-telemetry-platform-tfstate --versioning
```

`terraform init` then uses the GCS backend automatically. Storage cost for the ~25 KB state file is effectively zero.

### Teardown (demo-at-rest)

```bash
./destroy.sh   # destroys Cloud Run/Scheduler; preserves Firestore data + secrets
```

Idle cost after teardown is effectively zero. Re-run `./deploy.sh` to bring the demo back online.

---

## Operations

- **User guide:** `dashboard/public/docs/help.md` (in-app `📖`/`❓`).
- **Incident runbook:** `dashboard/public/docs/runbook.md` (SRE procedures, alerts, escalation).

Both are served at `/docs/*.md` from the built SPA.
