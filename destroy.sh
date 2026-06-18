#!/bin/bash
set -e

PROJECT_ID="gcp-telemetry-platform"
REGION="europe-west3" # Frankfurt

echo "🧹 Starting GCP Telemetry Platform Teardown (Demo-at-Rest Mode)..."

# Navigate to infra directory
cd "$(dirname "$0")/infra"

# Run Terraform Destroy
echo "1️⃣ Tearing down managed infrastructure..."
terraform init
terraform destroy -auto-approve \
    -var="project_id=$PROJECT_ID" \
    -var="region=$REGION" \
    -var="image_url=dummy-image" # Image doesn't matter for destroy

echo "✅ Teardown Complete!"
echo "Note: Secrets in Secret Manager and data in Firestore have been preserved."
echo "Monthly idle cost estimate: < \$0.10"
