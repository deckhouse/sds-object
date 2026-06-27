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
| `ObjectBucket` (`ob`) | Namespaced | Creates a bucket and writes a standard S3 credentials `Secret` next to your application. |

`ObjectBucket` is namespaced on purpose: an application team creates a bucket in its own namespace, and the generated access key lands in a `Secret` in that namespace, so access is naturally scoped by RBAC.

## Cluster profiles

A single `spec.type` selects the profile; the backend is chosen and operated for you.

| `spec.type` | Backend | Placement / data | Use case |
|-------------|---------|------------------|----------|
| `System` | Garage | DaemonSet on control-plane nodes, `hostPath` | Platform/system needs (backups, registry, logs). |
| `Lightweight` | Garage | StatefulSet, PVC on a StorageClass | Small application workloads. |
| `Full` | SeaweedFS | StatefulSet, PVC | Scalable, full-featured storage. |
| `Heavy` | Ceph RGW | RADOS Gateway on an existing sds-elastic cluster | Reuses Ceph capacity and fault tolerance. |

{{< alert level="info" >}}
Implementation status: the `System` and `Lightweight` profiles (Garage) are available. `Full` (SeaweedFS) and `Heavy` (Ceph RGW) are planned and not yet functional.
{{< /alert >}}

## How it works

- The controller reconciles each `ObjectStorageCluster` into a backend data plane (workloads, services, configuration) and reports readiness, the S3 endpoint, and capacity in `status`.
- The controller reconciles each `ObjectBucket` into a bucket and a scoped access key on the referenced cluster, then writes a `Secret` (owned by the `ObjectBucket`) with the standard connection variables: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.

See [Usage](usage.html) for a walkthrough.

## Requirements

- The `Heavy` profile requires the [`sds-elastic`](/modules/sds-elastic/) module with a ready `ElasticCluster`; it is consulted only for `Heavy` clusters.
- The `System`, `Lightweight`, and `Full` profiles are self-contained (the backend images are shipped with the module).
