# Testing manifests

Ready-to-apply manifests for smoke-testing the implemented `sds-object`
profiles on a real cluster. Everything lands in the `obj-test` namespace.

Implemented today:

- **System** / **Lightweight** (Garage) — full flow: cluster → bucket →
  credentials `Secret` → a `Job` that writes and reads an object via `mc`.
- **Full** (SeaweedFS) — single-node MVP: cluster → a `Job` that checks the S3
  endpoint responds. Bucket/credential provisioning is **not implemented yet**
  for Full, so there is no `ObjectBucket`/`Secret` for it.

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

# Full (SeaweedFS) — endpoint smoke test only
kubectl apply -f testing/30-full.yaml
kubectl -n obj-test logs job/s3-smoke-full
```

A test `Job` succeeds when its log ends with `S3 OK`.

## Cleanup

```shell
kubectl delete ns obj-test    # buckets use reclaimPolicy: Delete (data removed)
kubectl delete objectstoragecluster system lightweight full
```
