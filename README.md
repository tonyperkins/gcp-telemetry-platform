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

   Off-cloud flight pusher ──▶ POST /ingest/flight/push  (Bearer token)
   (residential IP)              │
                                 ▲ OpenSky API (fetch from unblocked IP)

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
| Build | GitHub Actions (CI/CD) / Cloud Build (`deploy.sh`) |

**No VPC / Cloud NAT.** An earlier design ran a Serverless VPC Access connector + Cloud NAT + reserved static IP to give OpenSky a stable egress IP. OpenSky blocks GCP datacenter IPs regardless of region or static IP (see [Known limitations](#known-limitations)), so that stack cost ~$45/mo for zero benefit and has been removed. Flights now arrive via the off-cloud pusher, and the Metro feed is reachable over Cloud Run's default direct internet egress.

### Region choice

Resources deploy to **`europe-west3` (Frankfurt)**, set in `deploy.sh`/`destroy.sh`. The original rationale was OpenSky proximity (its API servers are in Switzerland) and EU IP reputation. **This does not actually help** (see below), but the region is otherwise fine and kept for continuity. Note the Terraform `region` variable still defaults to `us-central1`; the scripts override it explicitly.

### Cost

Designed to cost **a few dollars a month or less**:

- **Cloud Run** scales to zero (`min_instance_count = 0`, `cpu_idle = true`) and is capped at 2 instances, so there is no idle compute charge and no runaway-bill risk.
- **Metro polling** runs every 2 minutes (`metro_polling_cron`), keeping Cloud Run invocations and Firestore writes inside the free tier.
- **Firestore** documents carry a 24h TTL (`expire_at` field), so the append-only `vehicles` collection self-prunes and storage stays in the free tier.
- **No always-on networking** (VPC connector / Cloud NAT / static IP removed) — this was the single largest line item.
- **Artifact Registry** has a cleanup policy (keep 5 most recent images, delete older) set by `infra/bootstrap-cicd.sh`, so old deploy images never accumulate.

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
├── cmd/server/main.go              # Entry point: router, DI, static file server
├── internal/
│   ├── api/                        # HTTP handlers
│   │   ├── api_handlers.go         # Public dashboard API (vehicles, routes, health, logs)
│   │   ├── worker_handlers.go      # /ingest endpoints invoked by Cloud Scheduler + push
│   │   ├── worker_handlers_test.go # Tests for push auth + payload validation
│   │   └── management_handlers.go  # Pause/resume Cloud Scheduler jobs
│   ├── services/                   # Ingestion + GTFS logic
│   │   ├── flight_ingestion.go     # OpenSky fetch (OAuth2 + circuit breaker)
│   │   ├── flight_ingestion_test.go# Tests for flight parsing/normalization
│   │   ├── metro_ingestion.go      # GTFS-RT protobuf parsing
│   │   └── gtfs_shapes.go          # Static GTFS route shapes
│   └── data/                       # Firestore repository + models
│       ├── firestore_repository.go # Batch writes, deduplicated reads, TTL
│       └── models.go               # Unified Vehicle schema
├── dashboard/                      # React + TypeScript + Vite + Leaflet
│   ├── src/
│   │   ├── App.tsx                 # Root component, layout, state orchestration
│   │   ├── components/             # Map, markers, SRE sidebar, controls, modals (21 files)
│   │   ├── hooks/                  # Vehicle data, routes, flight trails, metrics, etc.
│   │   ├── types/                  # TypeScript type definitions
│   │   ├── utils/                  # API client, formatting, map helpers
│   │   └── styles/                 # CSS
│   ├── public/docs/                # Served at /docs/* by the SPA
│   │   ├── help.md                 # In-app user guide (📖 button)
│   │   └── runbook.md              # SRE incident runbook (mermaid diagrams)
│   └── dist/                       # Build output baked into the container
├── deploy/
│   └── homebox/                    # Off-cloud flight pusher (Docker Compose)
│       ├── cmd/flightpusher/       # Pusher entry point
│       ├── internal/               # Reuses server's services + data packages
│       ├── compose.yaml            # Docker Compose for the pusher
│       ├── Dockerfile.pusher       # Pusher container image
│       ├── .env.example            # Template for pusher env vars
│       └── README.md               # Pusher setup + security notes
├── .github/workflows/
│   ├── ci.yml                      # PR/push: go build/vet/test, dashboard build, terraform fmt/validate
│   ├── terraform-plan.yml          # PRs touching infra/**: plan posted as PR comment
│   └── deploy.yml                  # Manual: build + push image, terraform apply
├── infra/                          # Terraform (Cloud Run, Scheduler, Firestore TTL, IAM)
│   ├── main.tf                     # Cloud Run service, Scheduler job, IAM, Firestore TTL
│   ├── variables.tf                # Input variables (project_id, region, image_url, etc.)
│   ├── outputs.tf                  # Service URL, project ID, region
│   ├── backend.tf                  # Remote state (GCS)
│   └── bootstrap-cicd.sh           # One-time WIF + service-account setup
├── Dockerfile                      # Multi-stage: build frontend + backend
├── deploy.sh                       # Local/break-glass: build image + terraform apply
└── destroy.sh                      # terraform destroy (demo-at-rest teardown)
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
| POST | `/api/manage/heartbeat` | No-op heartbeat (keeps frontend console clean) |
| POST | `/api/manage/start` \| `/api/manage/stop` | Resume / pause Cloud Scheduler jobs |
| GET | `/api/debug/inspect` | Raw document counts + sample (break-glass) |
| GET | `/api/logs/{source}` | Server log viewer (filtered by source: metro/flight/api/all) |
| POST | `/ingest/metro` | Metro worker trigger (Cloud Scheduler, OIDC) |
| POST | `/ingest/flight` | Flight worker trigger (Cloud Scheduler, OIDC; ineffective from GCP — see Known limitations) |
| POST | `/ingest/flight/push` | Off-cloud flight push endpoint (Bearer token via `INGEST_PUSH_TOKEN`) |

---

## Local development

### Prerequisites

- Go 1.25+
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
| `GCP_PROJECT_ID` | `demo-project` | Project ID for Cloud Scheduler job names (management handlers) |
| `FIRESTORE_DB_ID` | `(default)` | Firestore database ID |
| `GCP_LOCATION` | `us-central1` | Region for Cloud Scheduler job names |
| `OPENSKY_BBOX` | `29.8,-98.2,30.8,-97.2` | `lamin,lomin,lamax,lomax` |
| `OPENSKY_CLIENT_ID` / `OPENSKY_CLIENT_SECRET` | _(unset)_ | OpenSky OAuth2 (4000 credits/day); anonymous otherwise |
| `INGEST_PUSH_TOKEN` | _(unset)_ | Bearer token guarding `POST /ingest/flight/push` |
| `METRO_GTFS_RT_URL` | CapMetro feed | GTFS-RT source |
| `LOG_FILE_PATH` | `/tmp/logs/out.log` | Server log file (served by `/api/logs/{source}`) |
| `MANAGEMENT_ADMIN_TOKEN` | _(unset)_ | Bearer token guarding `/api/manage/start\|stop` |

### Frontend

```bash
cd dashboard
npm install
npm run dev      # Vite dev server; proxies API to VITE_API_BASE_URL (.env.development)
npm run build    # Outputs to dashboard/dist (served by the Go binary in prod)
```

The frontend is **React 18 + TypeScript + Vite + Leaflet**, with `marked` for rendering the in-app markdown docs and `mermaid` for runbook sequence diagrams.

### Testing

```bash
# Go tests (backend)
go test ./...

# Frontend build check (TypeScript compile + Vite bundle)
cd dashboard && npm run build
```

Tests live alongside source files (`*_test.go`). CI runs both on every PR.

---

## Deployment

The primary path is **CI/CD via GitHub Actions** (below). `deploy.sh` remains as a local/break-glass alternative.

### CI/CD (GitHub Actions)

| Workflow | Trigger | Auth | Does |
|---|---|---|---|
| `ci.yml` | every PR + push to `main` | none | `go build`/`vet`/`test`, dashboard build, `terraform fmt`/`validate` |
| `terraform-plan.yml` | PRs touching `infra/**` | WIF → `gh-planner` (read-only) | `terraform plan`, posted as a PR comment |
| `deploy.yml` | manual (`workflow_dispatch`) | WIF → `gh-deployer` | Docker build + push to Artifact Registry, then `terraform apply` |

**Keyless auth via Workload Identity Federation.** No service-account JSON keys exist; GitHub's OIDC token is exchanged for short-lived GCP credentials. The WIF provider is locked to this repo (`attribute.repository`), and the two identities are least-privilege:

- **`gh-planner`** — `roles/viewer` only; impersonable from any ref, so PR plans can run safely.
- **`gh-deployer`** — curated, non-escalating roles (Cloud Run / Scheduler / Artifact Registry / Firestore + state-bucket access, plus `actAs` on the runtime SAs, and read-only IAM visibility for state refresh). Deliberately *lacks* project-IAM-admin, SA-admin and secret-admin, so CI can deploy images but cannot modify IAM, service accounts or secrets — those stay a human-run `deploy.sh` operation. Impersonation is restricted to `refs/heads/main`.

**Deploys are gated.** `deploy.yml` only runs from the manual *Run workflow* button, and the `production` GitHub Environment requires a reviewer's approval before the job executes — a merge never spins up paid resources on its own.

One-time setup of the WIF pool, providers and service accounts is scripted in **`infra/bootstrap-cicd.sh`** (idempotent; run once with an admin account). Repo variables (`WIF_PROVIDER`, `GCP_*`) wire the workflows to the project.

### Infrastructure (Terraform)

All infrastructure is defined in `infra/` as a single Terraform module using the `hashicorp/google` provider (`~> 5.0`).

#### Resources created

| Resource | Type | Purpose |
|---|---|---|
| `telemetry-api` | `google_cloud_run_v2_service` | Go binary serving API + SPA; scales 0→2, CPU idle |
| `telemetry-cloudrun-sa` | `google_service_account` | Cloud Run runtime identity |
| `telemetry-scheduler-sa` | `google_service_account` | Cloud Scheduler invoker identity (OIDC) |
| `metro-ingester` | `google_cloud_scheduler_job` | Polls CapMetro GTFS-RT every 2 min (configurable) |
| `vehicles_ttl` | `google_firestore_field` | 24h TTL on `vehicles` collection (auto-expire) |
| Secret IAM (×3) | `google_secret_manager_secret_iam_member` | Grants Cloud Run SA `secretAccessor` on each secret |
| `scheduler_admin` | `google_project_iam_member` | `roles/cloudscheduler.admin` on Cloud Run SA (pause/resume) |
| `firestore_user` | `google_project_iam_member` | `roles/datastore.user` on Cloud Run SA |
| `public` | `google_cloud_run_service_iam_member` | `roles/run.invoker` to `allUsers` (public dashboard + API) |

Secrets themselves (`OPENSKY_CLIENT_ID`, `OPENSKY_CLIENT_SECRET`, `INGEST_PUSH_TOKEN`) are **data sources**, not resources — they must exist in Secret Manager before `terraform apply` (`deploy.sh` creates placeholders if missing).

#### Service accounts & IAM

| SA | Used by | Roles | Why |
|---|---|---|---|
| `telemetry-cloudrun-sa` | Cloud Run runtime | `secretmanager.secretAccessor`, `cloudscheduler.admin`, `datastore.user` | Read secrets, pause/resume scheduler jobs via management API, read/write Firestore |
| `telemetry-scheduler-sa` | Cloud Scheduler | (none beyond OIDC invoker) | Authenticates scheduler→Cloud Run calls via OIDC token |
| `gh-planner` | GitHub Actions (PRs) | `viewer`, `secretmanager.viewer` | Read-only terraform plan on PRs |
| `gh-deployer` | GitHub Actions (main) | `run.admin`, `cloudscheduler.admin`, `artifactregistry.writer`, `datastore.owner`, `secretmanager.viewer`, `iam.securityReviewer`, `serviceusage.serviceUsageConsumer` + `actAs` on runtime SAs + state bucket objectAdmin | Build, push image, terraform apply. Deliberately lacks IAM/SA/secret admin |

#### Required GitHub repo variables

After running `infra/bootstrap-cicd.sh`, set these in **GitHub → Settings → Secrets and variables → Actions → Variables**:

| Variable | Example | Used by |
|---|---|---|
| `WIF_PROVIDER` | `projects/992.../locations/global/workloadIdentityPools/github-pool/providers/github-provider` | `deploy.yml`, `terraform-plan.yml` |
| `GCP_PLANNER_SA` | `gh-planner@gcp-telemetry-platform.iam.gserviceaccount.com` | `terraform-plan.yml` |
| `GCP_DEPLOYER_SA` | `gh-deployer@gcp-telemetry-platform.iam.gserviceaccount.com` | `deploy.yml` |
| `GCP_PROJECT_ID` | `gcp-telemetry-platform` | `deploy.yml`, `terraform-plan.yml` |
| `GCP_REGION` | `europe-west3` | `deploy.yml`, `terraform-plan.yml` |
| `GCP_AR_REPO` | `telemetry-repo` | `deploy.yml` |
| `GCP_IMAGE_NAME` | `telemetry-app` | `deploy.yml` |

#### Customization

| Variable | Default | Override |
|---|---|---|
| `project_id` | _(required)_ | `deploy.sh` / `TF_VAR_project_id` |
| `region` | `us-central1` | `deploy.sh` sets `europe-west3` / `TF_VAR_region` |
| `image_url` | _(required)_ | `deploy.sh` / `TF_VAR_image_url` |
| `opensky_bbox` | `29.8,-98.2,30.8,-97.2` (Austin) | `TF_VAR_opensky_bbox` |
| `metro_polling_cron` | `*/2 * * * *` (every 2 min) | `TF_VAR_metro_polling_cron` |

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
