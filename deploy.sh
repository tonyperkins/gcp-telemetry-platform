#!/bin/bash
set -e

PROJECT_ID="gcp-telemetry-platform"
REGION="europe-west3"
REPO_NAME="telemetry-repo"
IMAGE_NAME="telemetry-app"
IMAGE_TAG=$(date +%s) # Use timestamp instead of latest to force Terraform to deploy new revisions

echo "🚀 Starting GCP Telemetry Platform Deployment..."

echo "1️⃣ Enabling Required GCP APIs..."
# Note: compute.googleapis.com / vpcaccess.googleapis.com are no longer needed
# now that the VPC connector + Cloud NAT stack has been removed.
gcloud services enable \
    artifactregistry.googleapis.com \
    run.googleapis.com \
    cloudbuild.googleapis.com \
    secretmanager.googleapis.com \
    cloudscheduler.googleapis.com \
    firestore.googleapis.com \
    --project=$PROJECT_ID

echo "2️⃣ Checking if Artifact Registry repository exists..."
if ! gcloud artifacts repositories describe $REPO_NAME --location=$REGION --project=$PROJECT_ID > /dev/null 2>&1; then
    echo "Creating Docker repository: $REPO_NAME..."
    gcloud artifacts repositories create $REPO_NAME \
        --repository-format=docker \
        --location=$REGION \
        --description="Docker repository for Telemetry Platform" \
        --project=$PROJECT_ID
else
    echo "Repository $REPO_NAME already exists."
fi

# Define the full image URL
FULL_IMAGE_NAME="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"

echo "3️⃣ Initializing dummy secrets so Terraform doesn't crash if they don't exist..."
# Terraform fails if the secret payload has never been set when referencing it
for secret in "OPENSKY_CLIENT_ID" "OPENSKY_CLIENT_SECRET" "INGEST_PUSH_TOKEN"; do
    if ! gcloud secrets describe $secret --project=$PROJECT_ID > /dev/null 2>&1; then
        echo "Creating placeholder for secret: $secret"
        printf "pending" | gcloud secrets create $secret --data-file=- --project=$PROJECT_ID --replication-policy=automatic
    fi
done

echo "4️⃣ Submitting Cloud Build for monolithic container..."
# Build and push the container natively using Google Cloud Build
gcloud builds submit --tag $FULL_IMAGE_NAME --project=$PROJECT_ID

echo "5️⃣ Applying Terraform..."
cd infra
terraform init
terraform apply -auto-approve \
    -var="project_id=$PROJECT_ID" \
    -var="region=$REGION" \
    -var="image_url=$FULL_IMAGE_NAME"

echo "✅ Deployment Complete! Visit the Cloud Run URL shown above."
