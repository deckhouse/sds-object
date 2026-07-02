# E2E tests for sds-object

End-to-end coverage for the documented `ObjectStorageCluster` / `ObjectBucket`
lifecycle: cluster creation, bucket provisioning with the standardised S3
credentials `Secret`, a real S3 write/list/read round-trip through the generated
credentials, the admission guards (validating webhooks + CRD CEL rules), and
finalizer-driven deletion.

1. `storage-e2e` brings up a nested cluster from `tests/cluster_config.yml`
   (1 master + 2 workers) with the `sds-object` module enabled.
2. `BeforeSuite` waits for the module to become Ready and ensures the in-cluster
   test namespace exists.
3. A single shared `ObjectStorageCluster` is created and the ordered specs run
   on top of it: create → bucket + creds `Secret` + S3 round-trip → validation
   guards → delete.
4. `AfterSuite` hands the cluster back to `storage-e2e` for teardown.

## Running in CI (PR label)

The suite is wired into the reusable `storage-e2e` pipeline via
[`.github/workflows/e2e-tests.yml`](../.github/workflows/e2e-tests.yml). It is
**gated by the `e2e/run` PR label**: add that label to a pull request to trigger
a run (the job graph is `resolve → bootstrap → run-tests → teardown`). The
pipeline is configured with `cluster_provider: commander`, so each run creates a
fresh cluster through the Deckhouse Commander API and deletes it on teardown.

Other labels:

- `e2e/keep-cluster` — skip teardown so you can inspect / re-run on the cluster.
- `e2e/label:<suite>` — Ginkgo label filter (multiple are OR-joined).

The Commander endpoint, token and template come from inherited org/repo secrets
and vars (`E2E_COMMANDER_*`); see `storage-e2e` `docs/CI.md` for the full list.

Pipeline flow (`resolve → bootstrap → run-tests → teardown`): `bootstrap`
creates the cluster via Commander **and** enables `sds-object` (+ its
dependencies) from `tests/cluster_config.ci.yml`
(`modulePullOverride: "${E2E_MODULE_IMAGE_TAG}"`, resolved to `pr<N>`),
connecting in-process to the master (SSH via the bastion + API tunnel);
`run-tests` attaches to that cluster the same way (the commander provider's
`Connect`) and runs the suite. This requires the PR's dev image (`pr<N>`) to be
built and pushed to the dev-registry **before** the e2e run (the `build_dev`
workflow), and the Commander cluster must be able to pull from that registry.

## Running locally

The suite drives the CRDs through the dynamic client and reads the
`storage.deckhouse.io/v1alpha1` Go types from the in-repo `sds-object/api`
module (`replace github.com/deckhouse/sds-object/api => ../api`). All generic
nested-cluster plumbing (`pkg/cluster`, `pkg/kubernetes`) comes from
`storage-e2e`, consumed as a pinned pseudo-version.

## Profiles

The shared cluster's profile is selectable via `E2E_OSC_TYPE` (default
`System`). The default is intentionally the cheapest, most self-contained
profile so CI needs no extra storage modules:

| `E2E_OSC_TYPE` | Backend | Extra requirements |
|----------------|---------|--------------------|
| `System` (default) | Garage (DaemonSet on control-plane, hostPath) | none |
| `Lightweight` | Garage (StatefulSet + PVC) | `E2E_STORAGE_CLASS` + a CSI/local-volume module enabled in `cluster_config.yml` |
| `Full` | SeaweedFS | `E2E_STORAGE_CLASS` + `managed-postgres` module |
| `Heavy` | Ceph RGW | `sds-elastic` module + a Ready `ElasticCluster` (`E2E_ELASTIC_CLUSTER_REF`) |

When you point `E2E_OSC_TYPE` at a heavier profile, enable the corresponding
modules in `tests/cluster_config.yml` first (see the comments there). The suite
fails fast in `BeforeSuite` if a profile's required env knob is missing.

## Why one shared cluster + Ordered specs

The validation and delete specs build on the cluster and bucket created by the
first specs, so the suite uses a **single shared `ObjectStorageCluster`** inside
one `Describe(..., Ordered)`. Spec registration goes through builder functions
called in explicit order from the root container
(`createSpecs → validationSpecs → deleteSpecs`); the deletion specs run last.
`RandomizeAllSpecs` stays **off**.

## Requirements

- Go **1.26+**
- A base Deckhouse cluster with the `virtualization` module enabled.
- SSH access to the master node of the base cluster.
- A Deckhouse license and a docker config for the dev registry.
- A block-mode `StorageClass` on the base cluster for the VM OS disks.
- Outbound access to Docker Hub for the `minio/mc` probe image (override with
  `E2E_PROBE_IMAGE` if you mirror it).

## Environment variables

### `storage-e2e` (nested cluster)

- `TEST_CLUSTER_CREATE_MODE` (**required**):
  one of `alwaysCreateNew`, `alwaysUseExisting`, `commander`.
- `TEST_CLUSTER_CLEANUP`:
  set to `true` to delete the VMs after the run.
- `TEST_CLUSTER_NAMESPACE`:
  the VM namespace on the base cluster **and** the in-cluster namespace the
  suite uses for ObjectBuckets / credentials Secrets / probe Pods (single source
  of truth — no separate `E2E_NAMESPACE`).
- `TEST_CLUSTER_STORAGE_CLASS`:
  base-cluster `StorageClass` for the VM OS disks.
- `YAML_CONFIG_FILENAME`:
  defaults to `cluster_config.yml`.
- `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`
- `SSH_PUBLIC_KEY`:
  SSH public key injected as the VMs' authorized key. **Required in
  `alwaysCreateNew` mode**.
- `SSH_VM_USER`:
  SSH user inside the created VMs (must match the VM image, usually `cloud`).
  **Required in `alwaysCreateNew` mode**.
- `SSH_JUMP_HOST`, `SSH_JUMP_USER`, `SSH_JUMP_KEY_PATH`:
  jump-host (bastion) SSH settings used by `alwaysUseExisting`.
- `TEST_CLUSTER_FORCE_LOCK_RELEASE`:
  set to `true` to steal a stale `e2e-cluster-lock` left by a crashed run.
- `DKP_LICENSE_KEY`
- `REGISTRY_DOCKER_CFG`
- `SDS_OBJECT_MODULE_PULL_OVERRIDE`:
  overrides `modulePullOverride` for `sds-object` from
  `tests/cluster_config.yml` (which keeps a literal `main` default). Set to
  `prN` on GitHub, `mrN` on GitLab, or `main` for nightly. (This is
  storage-e2e's generic per-module convention: `<MODULE>_MODULE_PULL_OVERRIDE`.)

### `sds-object` suite knobs

- `E2E_OSC_NAME`: name of the shared `ObjectStorageCluster`, defaults to `e2e-osc`.
- `E2E_OSC_TYPE`: profile, one of `System` (default) / `Lightweight` / `Full` / `Heavy`.
- `E2E_REDUNDANCY`: `Single` (default) / `Replicated` / `HighRedundancy`.
- `E2E_STORAGE_CLASS`: StorageClass for the PVCs; **required** for `Lightweight`/`Full`.
- `E2E_OSC_SIZE`: cluster storage size, defaults to `5Gi`.
- `E2E_ELASTIC_CLUSTER_REF`: `ElasticCluster` name; **required** for `Heavy`.
- `E2E_BUCKET_NAME`: name of the shared `ObjectBucket`, defaults to `e2e-bucket`.
- `E2E_OSC_READY_TIMEOUT`: Go duration bounding the cluster Ready wait, defaults to 15m.
- `E2E_OB_READY_TIMEOUT`: Go duration bounding the bucket Ready wait, defaults to 5m.
- `E2E_MODULE_READY_TIMEOUT`: Go duration bounding the module Ready wait, defaults to 15m.
- `E2E_PROBE_IMAGE`: container image carrying `mc` for the S3 round-trip Job, defaults to `minio/mc:latest`.
- `E2E_PROBE_JOB_TIMEOUT`: Go duration bounding the probe Job, defaults to 5m.
- `E2E_KEEP_CLUSTER_ON_FAILURE`: when truthy and at least one spec failed, the
  nested cluster is **not** torn down in `AfterSuite`, so you can inspect it.

## Quick start

```bash
export TEST_CLUSTER_CREATE_MODE=alwaysCreateNew
export TEST_CLUSTER_CLEANUP=true
export TEST_CLUSTER_NAMESPACE=e2e-sds-object
export TEST_CLUSTER_STORAGE_CLASS=linstor-r2

export SSH_HOST=<master-ip>
export SSH_USER=<ssh-user>
export SSH_PRIVATE_KEY=~/.ssh/id_rsa
export SSH_PUBLIC_KEY=~/.ssh/id_rsa.pub   # required for alwaysCreateNew
export SSH_VM_USER=cloud                  # required for alwaysCreateNew

export DKP_LICENSE_KEY=<license>
export REGISTRY_DOCKER_CFG=<base64-docker-config>

# Override the sds-object image tag; optional, defaults to the literal "main".
export SDS_OBJECT_MODULE_PULL_OVERRIDE=main   # or prN / mrN to test a specific PR/MR

cd e2e
make deps
make test
```

For local debugging you can run a subset of specs:

```bash
make test-focus FOCUS="round-trip"
```

## Compile check (no cluster)

```bash
make build
make vet
```
