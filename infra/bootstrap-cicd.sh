#!/bin/bash
# ---------------------------------------------------------------------------
# One-time bootstrap for GitHub Actions CI/CD via Workload Identity Federation.
#
# Idempotent: safe to re-run. Creates two least-privilege service accounts and
# a WIF provider locked to this repo, so GitHub Actions can authenticate to GCP
# with short-lived OIDC tokens — no exported service-account JSON keys.
#
#   gh-planner   read-only; impersonable from any ref in this repo -> terraform plan on PRs
#   gh-deployer  curated deploy roles; impersonable ONLY from refs/heads/main -> build + apply
#
# Run once, locally, with an account that has Owner/IAM admin on the project.
# ---------------------------------------------------------------------------
set -euo pipefail

PROJECT_ID="gcp-telemetry-platform"
PROJECT_NUMBER="992691572461"
REGION="europe-west3"
REPO="tonyperkins/gcp-telemetry-platform"

POOL="github-pool"
PROVIDER="github-provider"
PLANNER_SA="gh-planner@${PROJECT_ID}.iam.gserviceaccount.com"
DEPLOYER_SA="gh-deployer@${PROJECT_ID}.iam.gserviceaccount.com"

STATE_BUCKET="gcp-telemetry-platform-tfstate"
CLOUDRUN_SA="telemetry-cloudrun-sa@${PROJECT_ID}.iam.gserviceaccount.com"
SCHEDULER_SA="telemetry-scheduler-sa@${PROJECT_ID}.iam.gserviceaccount.com"

POOL_RES="projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL}"

echo "1/6 Enabling required APIs..."
# cloudresourcemanager is required for Terraform to read/manage project IAM
# policy under the CI service-account identity.
gcloud services enable \
    iamcredentials.googleapis.com \
    sts.googleapis.com \
    iam.googleapis.com \
    cloudresourcemanager.googleapis.com \
    --project="$PROJECT_ID"

echo "2/6 Creating service accounts (idempotent)..."
gcloud iam service-accounts describe "$PLANNER_SA" --project="$PROJECT_ID" >/dev/null 2>&1 || \
    gcloud iam service-accounts create gh-planner --project="$PROJECT_ID" \
        --display-name="GitHub Actions - read-only planner (PRs)"
gcloud iam service-accounts describe "$DEPLOYER_SA" --project="$PROJECT_ID" >/dev/null 2>&1 || \
    gcloud iam service-accounts create gh-deployer --project="$PROJECT_ID" \
        --display-name="GitHub Actions - deployer (main only)"

echo "3/6 Granting PLANNER read-only roles..."
for role in roles/viewer roles/secretmanager.viewer; do
    gcloud projects add-iam-policy-binding "$PROJECT_ID" \
        --member="serviceAccount:${PLANNER_SA}" --role="$role" --condition=None >/dev/null
done

echo "4/6 Granting DEPLOYER curated (non-escalating) roles..."
# Deliberately excludes projectIamAdmin / serviceAccountAdmin / secretmanager.admin:
# the IAM bindings, service accounts and secrets are managed out-of-band, so a
# normal image deploy never needs to modify them. iam.securityReviewer grants
# READ-only IAM visibility (getIamPolicy) so Terraform can refresh the
# google_project_iam_member resources — it cannot write IAM. If those resources
# ever change, `apply` fails loudly and a human runs ./deploy.sh — no silent
# privilege escalation path from CI.
for role in \
    roles/run.admin \
    roles/cloudscheduler.admin \
    roles/artifactregistry.writer \
    roles/datastore.owner \
    roles/secretmanager.viewer \
    roles/iam.securityReviewer \
    roles/serviceusage.serviceUsageConsumer; do
    gcloud projects add-iam-policy-binding "$PROJECT_ID" \
        --member="serviceAccount:${DEPLOYER_SA}" --role="$role" --condition=None >/dev/null
done

# State bucket: object read/write is enough for the gcs backend (incl. lock).
# (The CI image is built on the runner and pushed to Artifact Registry, so no
# Cloud Build API / staging bucket access is needed.)
gcloud storage buckets add-iam-policy-binding "gs://${STATE_BUCKET}" \
    --member="serviceAccount:${DEPLOYER_SA}" --role="roles/storage.objectAdmin" >/dev/null

# actAs the runtime SAs (deploying Cloud Run / Scheduler OIDC requires it).
for sa in "$CLOUDRUN_SA" "$SCHEDULER_SA"; do
    gcloud iam service-accounts add-iam-policy-binding "$sa" --project="$PROJECT_ID" \
        --member="serviceAccount:${DEPLOYER_SA}" --role="roles/iam.serviceAccountUser" >/dev/null
done

echo "5/6 Creating Workload Identity pool + provider (locked to ${REPO})..."
gcloud iam workload-identity-pools describe "$POOL" --project="$PROJECT_ID" --location=global >/dev/null 2>&1 || \
    gcloud iam workload-identity-pools create "$POOL" --project="$PROJECT_ID" --location=global \
        --display-name="GitHub Actions pool"

gcloud iam workload-identity-pools providers describe "$PROVIDER" \
    --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL" >/dev/null 2>&1 || \
    gcloud iam workload-identity-pools providers create-oidc "$PROVIDER" \
        --project="$PROJECT_ID" --location=global --workload-identity-pool="$POOL" \
        --display-name="GitHub OIDC" \
        --issuer-uri="https://token.actions.githubusercontent.com" \
        --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
        --attribute-condition="assertion.repository == '${REPO}'"

echo "6/6 Binding repo identities to the service accounts..."
# Planner: any ref in the repo (so PRs from branches can plan).
gcloud iam service-accounts add-iam-policy-binding "$PLANNER_SA" --project="$PROJECT_ID" \
    --role="roles/iam.workloadIdentityUser" \
    --member="principalSet://iam.googleapis.com/${POOL_RES}/attribute.repository/${REPO}" >/dev/null
# Deployer: ONLY workflows running on refs/heads/main (incl. workflow_dispatch on main).
gcloud iam service-accounts add-iam-policy-binding "$DEPLOYER_SA" --project="$PROJECT_ID" \
    --role="roles/iam.workloadIdentityUser" \
    --member="principalSet://iam.googleapis.com/${POOL_RES}/attribute.ref/refs/heads/main" >/dev/null

echo
echo "Done. Set these as GitHub repo variables (see infra/bootstrap-cicd.sh footer):"
echo "  WIF_PROVIDER = ${POOL_RES}/providers/${PROVIDER}"
echo "  GCP_PLANNER_SA = ${PLANNER_SA}"
echo "  GCP_DEPLOYER_SA = ${DEPLOYER_SA}"
