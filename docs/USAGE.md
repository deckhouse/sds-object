---
title: "Usage"
description: "Deploying object storage with sds-object: enabling the module, declaring an ObjectStorageCluster, creating an ObjectBucket, and consuming the generated credentials Secret."
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

`ObjectBucket` is namespaced — create it in your application's namespace:

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectBucket
metadata:
  name: app-data
  namespace: my-app
spec:
  clusterRef: shared
  # bucketName defaults to metadata.name
  accessPolicy: Private
  reclaimPolicy: Retain
```

Once `Ready`, the controller writes a `Secret` named `<bucket>-s3-credentials` in the same namespace:

```shell
d8 k -n my-app get objectbucket app-data
# NAME       CLUSTER   BUCKET     PHASE   SECRET                  READY   AGE
# app-data   shared    app-data   Ready   app-data-s3-credentials True    30s
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

## Reclaim policy

- `reclaimPolicy: Retain` (default) — deleting the `ObjectBucket` removes the access key but keeps the bucket and its objects.
- `reclaimPolicy: Delete` — deleting the `ObjectBucket` deletes the bucket and all its objects.

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
The `Heavy` profile provisions the Ceph RGW data plane (a Rook CephObjectStore on the referenced sds-elastic cluster) and reports the S3 endpoint once it is ready. Bucket/credential provisioning for `Heavy` (`ObjectBucket`) is a follow-up.
{{< /alert >}}
