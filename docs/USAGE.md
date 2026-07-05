---
title: "Usage"
description: "Deploying object storage with sds-object: enabling the module, declaring an ObjectStorageCluster, creating a cluster-scoped ObjectStorageBucket, granting namespaces access with ObjectStorageBucketPolicy, and consuming per-namespace credentials via ObjectStorageBucketAccess."
weight: 50
---

{{< alert level="warning" >}}
The `sds-object` module is in the `Experimental` stage. Experimental modules are not enabled by default. Set `allowExperimentalModules: true` in the `deckhouse` ModuleConfig before enabling the module. Currently only the `System` and `Lightweight` profiles (Garage) are functional.
{{< /alert >}}

## Enabling the module

```shell
d8 k apply -f - <<EOF
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: sds-object
spec:
  enabled: true
  version: 1
EOF
```

## Creating a cluster

A `Lightweight` cluster backed by PVCs on an existing StorageClass:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: shared
spec:
  type: Lightweight
  storage:
    size: 50Gi
    class: localpath
  redundancy: Replicated
```

A `System` cluster for platform needs (Garage on control-plane nodes, `hostPath`; `storage.class` is ignored):

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: system
spec:
  type: System
```

Track readiness:

```shell
d8 k get objectstoragecluster
# NAME     TYPE          PHASE   ENDPOINT                                           READY   AGE
# shared   Lightweight   Ready   http://shared-garage.d8-sds-object.svc...:3900     True    3m
```

## Creating a bucket

`ObjectStorageBucket` is **cluster-scoped** — it declares a bucket in a cluster but carries no credentials:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageBucket
metadata:
  name: app-data
spec:
  clusterRef: shared
  # bucketName defaults to metadata.name
  accessPolicy: Private
  reclaimPolicy: Retain
```

```shell
d8 k get objectstoragebucket app-data
# NAME       CLUSTER   BUCKET     PHASE   READY   AGE
# app-data   shared    app-data   Ready   True    30s
```

## Granting a namespace access

Access to a bucket is **deny-by-default**: a namespace can obtain credentials only when an `ObjectStorageBucketPolicy` for the bucket matches it. Namespaces are selected by exact `names` and/or RE2 `patterns`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageBucketPolicy
metadata:
  name: app-data-teams
spec:
  bucketRef: app-data
  allowedNamespaces:
    names:
      - my-app
    patterns:
      - "team-.*"
```

## Requesting credentials

Each consuming namespace declares an `ObjectStorageBucketAccess`. The controller mints a dedicated access key / secret key scoped to the bucket and writes a `Secret` (named `<access>-s3-credentials` by default) in the same namespace:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageBucketAccess
metadata:
  name: app-data
  namespace: my-app
spec:
  bucketRef: app-data
  permission: ReadWrite   # or ReadOnly
```

```shell
d8 k -n my-app get objectstoragebucketaccess app-data
# NAME       BUCKET     PHASE   SECRET                    READY   AGE
# app-data   app-data   Ready   app-data-s3-credentials   True    20s
```

## Consuming the credentials

The credentials `Secret` holds the standard S3 connection variables, ready to be mounted with `envFrom`:

| Key | Description |
|-----|-------------|
| `S3_ENDPOINT` | In-cluster S3 endpoint URL |
| `S3_REGION` | S3 region |
| `S3_BUCKET` | Bucket name |
| `AWS_ACCESS_KEY_ID` | Access key |
| `AWS_SECRET_ACCESS_KEY` | Secret key |

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: my-app
spec:
  template:
    spec:
      containers:
        - name: app
          image: my-app:latest
          envFrom:
            - secretRef:
                name: app-data-s3-credentials
```

## Rotating credentials

To rotate the access key of an `ObjectStorageBucketAccess`, set or change the `storage.deckhouse.io/rotate` annotation. The controller issues a fresh key pair, updates the `Secret`, and revokes the previous key:

```shell
d8 k -n my-app annotate objectstoragebucketaccess app-data \
  storage.deckhouse.io/rotate="$(date +%s)" --overwrite
```

## Reclaim policy

- Bucket `reclaimPolicy: Retain` (default) — deleting the `ObjectStorageBucket` keeps the bucket and its objects; `Delete` removes them.
- Deleting an `ObjectStorageBucketAccess` always revokes its access key and removes its credentials `Secret` (it does not touch bucket data).
- Cluster `reclaimPolicy: Retain` (default) — deleting the `ObjectStorageCluster` preserves persisted data (for `Heavy`, the Ceph RGW pools are kept; for PVC-backed profiles the PVCs are left in place). `Delete` destroys it.

## Heavy profile

The `Heavy` profile provisions a Ceph RADOS Gateway on top of an existing [`sds-elastic`](/modules/sds-elastic/) cluster and is selected with `spec.elasticClusterRef`:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: heavy
spec:
  type: Heavy
  elasticClusterRef: main
```

{{< alert level="info" >}}
The `Heavy` profile provisions the Ceph RGW data plane (a Rook CephObjectStore on the referenced sds-elastic cluster). Buckets and access work the same as for the other profiles: the `ObjectStorageBucket` creates a per-bucket owner Rook `CephObjectStoreUser` and the bucket, and each `ObjectStorageBucketAccess` gets its own `CephObjectStoreUser` granted on the bucket via a bucket policy.
{{< /alert >}}
