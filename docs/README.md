---
title: "Module sds-object"
description: "Deckhouse Kubernetes Platform module for managing S3-compatible object storage."
weight: 1
---

{{< alert level="warning" >}}
The module is in the `Experimental` stage. The API, configuration, and custom resources may change without notice; do not use it for production workloads.
{{< /alert >}}

The `sds-object` module manages S3-compatible object storage in a Deckhouse Kubernetes Platform cluster. It follows a no-ops philosophy: you declare *what* you need with two custom resources, and the module deploys and operates the backend for you. Low-level backend tuning is not exposed.

## Custom resources

| Resource | Scope | Purpose |
|----------|-------|---------|
| `ObjectStorageCluster` (`osc`) | Cluster | Provisions an object storage cluster of one of four turnkey profiles. |
| `ObjectStorageBucket` (`osb`) | Cluster | Declares a bucket in a cluster (no credentials). |
| `ObjectStorageBucketPolicy` (`osbp`) | Cluster | Declares which namespaces may request access to a bucket (deny-by-default). |
| `ObjectStorageBucketAccess` (`osba`) | Namespaced | Requests scoped credentials for a bucket; writes a standard S3 credentials `Secret` next to your application. |

Buckets are cluster-scoped; credentials are issued per namespace via `ObjectStorageBucketAccess`, gated by `ObjectStorageBucketPolicy` (an access whose namespace matches no policy stays pending). Each access gets its own access key, which can be rotated independently via the `storage.deckhouse.io/rotate` annotation.

## Cluster profiles

A single `spec.type` selects the profile; the backend is chosen and operated for you.

| `spec.type` | Backend | Placement / data | Use case |
|-------------|---------|------------------|----------|
| `System` | Garage | DaemonSet on control-plane nodes, `hostPath` | Platform/system needs (backups, registry, logs). |
| `Lightweight` | Garage | StatefulSet, PVC on a StorageClass | Small application workloads. |
| `Full` | SeaweedFS | StatefulSet, PVC | Scalable, full-featured storage. |
| `Heavy` | Ceph RGW | RADOS Gateway on an existing sds-elastic cluster | Reuses Ceph capacity and fault tolerance. |

{{< alert level="info" >}}
Implementation status: all four profiles are available. `Full` runs a distributed SeaweedFS (master/volume/filer + S3 gateway; replica counts and replication scale with `redundancy`). Filer metadata is kept in a shared PostgreSQL provisioned via the [managed-postgres](../../managed-postgres/stable/) module, so the filer/S3 gateway runs with multiple replicas (HA). `Heavy` provisions a Ceph RGW on an existing sds-elastic cluster. Buckets work on all four profiles.

The `Full` profile therefore requires the `managed-postgres` module to be enabled.
{{< /alert >}}

## How it works

- The controller reconciles each `ObjectStorageCluster` into a backend data plane (workloads, services, configuration) and reports readiness, the S3 endpoint, and capacity in `status`.
- The controller reconciles each `ObjectStorageBucket` into a bucket on the referenced cluster (no credentials).
- The controller reconciles each `ObjectStorageBucketAccess` — once an `ObjectStorageBucketPolicy` allows its namespace — into a scoped access key, then writes a `Secret` (owned by the access) with the standard connection variables: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.

See [Usage](usage.html) for a walkthrough.

## Requirements

- The `Heavy` profile requires the [`sds-elastic`](/modules/sds-elastic/) module with a ready `ElasticCluster`; it is consulted only for `Heavy` clusters.
- The `System`, `Lightweight`, and `Full` profiles are self-contained (the backend images are shipped with the module).
