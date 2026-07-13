---
title: "Module sds-object"
description: "Deckhouse Kubernetes Platform module for managing S3-compatible object storage."
weight: 1
---

{{< alert level="warning" >}}
The module is in the `Experimental` stage. The API, configuration, and custom resources may change without notice; do not use it for production workloads.
{{< /alert >}}

The `sds-object` module manages S3-compatible object storage in a Deckhouse Kubernetes Platform cluster. It follows a no-ops philosophy: you declare *what* you need with a small set of COSI-aligned custom resources, and the module deploys and operates the backend for you. Low-level backend tuning is not exposed.

## Custom resources

| Resource | Scope | Purpose |
|----------|-------|---------|
| `ObjectStore` (`ostore`) | Cluster | Provisions an object storage instance of one of four turnkey profiles (deploys the data plane; outside COSI). |
| `Bucket` (`bkt`) | Cluster | The backing bucket: either administrator-declared (Shared) or provisioned for a `BucketClaim`. |
| `BucketClaim` (`bc`) | Namespaced | The tenant request for a bucket: greenfield (provisions its own private bucket) or brownfield (binds a Shared bucket, gated by `BucketClaimPolicy`). |
| `BucketAccess` (`ba`) | Namespaced | Requests scoped credentials for a `BucketClaim`; writes a standard S3 credentials `Secret` next to your application. |
| `BucketClaimPolicy` (`bcp`) | Cluster | Declares which namespaces may bind a Shared bucket via a brownfield `BucketClaim` (deny-by-default). |

The model mirrors COSI (`Bucket` + `BucketClaim`) with two additions: `ObjectStore` deploys the storage itself, and `BucketClaimPolicy` governs cross-namespace sharing of Shared buckets. Credentials are issued per namespace via `BucketAccess` against a `BucketClaim` in the same namespace; each access gets its own access key, rotatable independently via the `storage.deckhouse.io/rotate` annotation.

## Cluster profiles

A single `spec.type` selects the profile; the backend is chosen and operated for you.

| `spec.type` | Backend | Placement / data | Use case |
|-------------|---------|------------------|----------|
| `System` | Garage | StatefulSet (fixed 3 replicas) on control-plane nodes, node-sticky local PV | Platform/system needs (backups, registry, logs). |
| `Lightweight` | Garage | StatefulSet, PVC on a StorageClass | Small application workloads. |
| `Full` | SeaweedFS | StatefulSet, PVC | Scalable, full-featured storage. |
| `Heavy` | Ceph RGW | RADOS Gateway on an existing sds-elastic cluster | Reuses Ceph capacity and fault tolerance. |

{{< alert level="info" >}}
Implementation status: all four profiles are available. `Full` runs a distributed SeaweedFS (master/volume/filer + S3 gateway; replica counts and replication scale with `redundancy`). Filer metadata is kept in a shared PostgreSQL provisioned via the [managed-postgres](../../managed-postgres/stable/) module, so the filer/S3 gateway runs with multiple replicas (HA). `Heavy` provisions a Ceph RGW on an existing sds-elastic cluster. Buckets work on all four profiles.

The `Full` profile therefore requires the `managed-postgres` module to be enabled.
{{< /alert >}}

## How it works

- The controller reconciles each `ObjectStore` into a backend data plane (workloads, services, configuration) and reports readiness, the S3 endpoint, and capacity in `status`.
- The controller reconciles each `Bucket` into a bucket on the referenced object store (no credentials).
- The controller reconciles each `BucketClaim`: a greenfield claim provisions its own private `Bucket`; a brownfield claim binds an existing Shared `Bucket` once a `BucketClaimPolicy` allows its namespace.
- The controller reconciles each `BucketAccess` — once its `BucketClaim` is Bound — into a scoped access key, then writes a `Secret` (owned by the access) with the standard connection variables: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.

See [Usage](usage.html) for a walkthrough.

## Requirements

- The `Heavy` profile requires the [`sds-elastic`](/modules/sds-elastic/) module with a ready `ElasticCluster`; it is consulted only for `Heavy` clusters.
- The `System`, `Lightweight`, and `Full` profiles are self-contained (the backend images are shipped with the module).
