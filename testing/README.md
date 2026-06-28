# Testing manifests

Ready-to-apply manifests for smoke-testing the implemented `sds-object`
profiles on a real cluster. Everything lands in the `obj-test` namespace.

All four profiles run the full flow: cluster → bucket → credentials `Secret` →
a `Job` that writes and reads an object via `mc`:

- **System** / **Lightweight** (Garage).
- **Full** (SeaweedFS) — distributed master/volume/filer.
- **Heavy** (Ceph RGW) — requires the `sds-elastic` module with a ready
  `ElasticCluster` (set `elasticClusterRef` in `40-heavy.yaml`).

## Notes

- The examples use `redundancy: Single` so they work on a cluster of any size.
  Use `Replicated` (≥3 nodes) for real redundancy.
- For Lightweight, set `spec.storage.class` to a StorageClass that exists in
  your cluster (`kubectl get sc`). The file uses `localpath` as a placeholder.
- The `mc` / `curl` test images are pulled from Docker Hub.

## Usage

Apply the namespace and a profile's cluster first, wait until it is `Ready`,
then apply the bucket + test job:

```shell
kubectl apply -f testing/00-namespace.yaml

# System
kubectl apply -f testing/10-system.yaml          # creates the cluster
kubectl get objectstoragecluster system -w        # wait for Ready
kubectl apply -f testing/10-system.yaml          # re-apply: bucket + job now provision
kubectl -n obj-test logs job/s3-test-system

# Lightweight (edit storage.class first)
kubectl apply -f testing/20-lightweight.yaml
kubectl -n obj-test logs job/s3-test-lightweight

# Full (SeaweedFS)
kubectl apply -f testing/30-full.yaml
kubectl -n obj-test logs job/s3-test-full

# Heavy (Ceph RGW) — needs sds-elastic + a ready ElasticCluster
kubectl apply -f testing/40-heavy.yaml
kubectl -n obj-test logs job/s3-test-heavy
```

A test `Job` succeeds when its log ends with `S3 OK`.

## Cleanup

```shell
kubectl delete ns obj-test    # buckets use reclaimPolicy: Delete (data removed)
kubectl delete objectstoragecluster system lightweight full
```
