# Remote state backend.
#
# State lives in a versioned, private GCS bucket with native state locking
# (supported by the gcs backend on modern Terraform), so concurrent applies
# can't corrupt it and accidental deletes are recoverable via object
# versioning. The bucket itself is created out-of-band (chicken-and-egg: it
# can't be managed by the state it stores) — see README "Remote state".
terraform {
  backend "gcs" {
    bucket = "gcp-telemetry-platform-tfstate"
    prefix = "telemetry/state"
  }
}
