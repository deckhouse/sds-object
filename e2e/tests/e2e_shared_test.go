/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tests

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	objectv1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/storage-e2e/pkg/cluster"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Suite env knobs (storage-e2e cluster knobs are read by storage-e2e itself) ---
const (
	envOSCName         = "E2E_OSC_NAME"
	envOSCType         = "E2E_OSC_TYPE"
	envRedundancy      = "E2E_REDUNDANCY"
	envStorageClass    = "E2E_STORAGE_CLASS"
	envPVCStorageClass = "E2E_PVC_STORAGE_CLASS"
	envOSCSize         = "E2E_OSC_SIZE"
	envElasticRef      = "E2E_ELASTIC_CLUSTER_REF"
	envBucketName      = "E2E_BUCKET_NAME"
	envOSCReadyTimeout = "E2E_OSC_READY_TIMEOUT"
	envOBReadyTimeout  = "E2E_OB_READY_TIMEOUT"
	envModuleReadyTO   = "E2E_MODULE_READY_TIMEOUT"
	envProbeImage      = "E2E_PROBE_IMAGE"
	envProbeJobTimeout = "E2E_PROBE_JOB_TIMEOUT"

	// envKeepClusterOnFailure, when truthy, skips nested-cluster teardown if any
	// spec failed, leaving the cluster live for manual debugging.
	envKeepClusterOnFailure = "E2E_KEEP_CLUSTER_ON_FAILURE"

	// envSkipSystemRecreate, when truthy, skips the singleReplica switch specs.
	// They are DESTRUCTIVE by design (toggling the setting recreates the system
	// store empty) and slow (two recreates), so a run that wants the system store
	// left intact can opt out.
	envSkipSystemRecreate = "E2E_SKIP_SYSTEM_RECREATE"
)

const (
	defaultOSCName = "e2e-osc"
	// systemOSCName is the name of the System ObjectStore the module
	// ships automatically (templates/system-object-storage.yaml). When the
	// primary profile is System the suite adopts this cluster instead of
	// creating a second one (a second System is denied by the webhook).
	systemOSCName      = "system"
	defaultOSCType     = string(objectv1alpha1.ClusterTypeSystem)
	defaultRedundancy  = string(objectv1alpha1.RedundancyNone)
	defaultOSCSize     = "5Gi"
	defaultBucketName  = "e2e-bucket"
	defaultProbeImage  = "minio/mc:latest"
	defaultNamespace   = "e2e-sds-object"
	defaultModuleReady = 15 * time.Minute
	defaultOSCReady    = 15 * time.Minute
	defaultOBReady     = 5 * time.Minute
	defaultProbeJobTO  = 5 * time.Minute

	// moduleName is the Deckhouse module / chart name under test; also the
	// suffix of its namespace (d8-sds-object).
	moduleName = "sds-object"
	moduleNS   = "d8-sds-object"
	apiGroup   = objectv1alpha1.APIGroup   // storage.deckhouse.io
	apiVersion = objectv1alpha1.APIVersion // v1alpha1
	probeAlias = "t"
)

const (
	pollInterval        = 5 * time.Second
	resourceGoneTimeout = 10 * time.Minute
)

var (
	objectStoreGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "objectstores",
	}
	// bucketGVR is cluster-scoped (used without .Namespace()).
	bucketGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "buckets",
	}
	// bucketClaimGVR is namespaced (used with .Namespace()).
	bucketClaimGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "bucketclaims",
	}
	// bucketAccessGVR is namespaced (used with .Namespace()).
	bucketAccessGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "bucketaccesses",
	}
	// bucketClaimPolicyGVR is cluster-scoped (used without .Namespace()).
	bucketClaimPolicyGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "bucketclaimpolicies",
	}

	// credsSecretKeys are the standardised keys the access reconciler writes into
	// the credentials Secret (BucketAccess.status.secretRef). The
	// suite asserts all are present and non-empty, and the probe Job envFroms the
	// Secret directly.
	credsSecretKeys = []string{
		objectv1alpha1.SecretKeyS3Endpoint,
		objectv1alpha1.SecretKeyS3Region,
		objectv1alpha1.SecretKeyS3Bucket,
		objectv1alpha1.SecretKeyAccessKeyID,
		objectv1alpha1.SecretKeySecretAccessID,
	}
)

type e2eConfig struct {
	// namespace is the in-cluster namespace for BucketAccesses /
	// credentials Secrets / probe Pods (Buckets are cluster-scoped).
	// Single source of truth: TEST_CLUSTER_NAMESPACE (also the base VM namespace).
	namespace string

	oscName         string
	oscType         string
	redundancy      string
	storageCl       string
	pvcStorageClass string
	oscSize         string
	elasticRef      string
	bucketName      string

	oscReadyTimeout time.Duration
	obReadyTimeout  time.Duration
	moduleReadyTO   time.Duration
	probeJobTimeout time.Duration

	probeImage string

	// keepClusterOnFailure, when true, makes cleanupSuite skip nested-cluster
	// teardown if any spec failed (E2E_KEEP_CLUSTER_ON_FAILURE).
	keepClusterOnFailure bool

	// skipSystemRecreate, when true, skips the destructive singleReplica switch
	// specs (E2E_SKIP_SYSTEM_RECREATE).
	skipSystemRecreate bool
}

var (
	suiteCfg              e2eConfig
	suiteRestCfg          *rest.Config
	suiteClientset        *clientgokube.Clientset
	suiteDyn              dynamic.Interface
	suiteClusterResources *cluster.TestClusterResources

	// oscCreatedBySuite records whether createSpecs actually created the primary
	// ObjectStore (true) or adopted a module-managed one such as the
	// shipped `system` cluster (false). deleteSpecs must not delete an adopted
	// cluster.
	oscCreatedBySuite bool
)

func loadConfig() e2eConfig {
	cfg := e2eConfig{
		namespace:       strings.TrimSpace(os.Getenv("TEST_CLUSTER_NAMESPACE")),
		oscName:         strings.TrimSpace(os.Getenv(envOSCName)),
		oscType:         strings.TrimSpace(os.Getenv(envOSCType)),
		redundancy:      strings.TrimSpace(os.Getenv(envRedundancy)),
		storageCl:       strings.TrimSpace(os.Getenv(envStorageClass)),
		pvcStorageClass: strings.TrimSpace(os.Getenv(envPVCStorageClass)),
		oscSize:         strings.TrimSpace(os.Getenv(envOSCSize)),
		elasticRef:      strings.TrimSpace(os.Getenv(envElasticRef)),
		bucketName:      strings.TrimSpace(os.Getenv(envBucketName)),
		probeImage:      strings.TrimSpace(os.Getenv(envProbeImage)),
	}

	if cfg.namespace == "" {
		cfg.namespace = defaultNamespace
	}
	if cfg.oscType == "" {
		cfg.oscType = defaultOSCType
	}
	if cfg.oscName == "" {
		// System has no self-created cluster: the module ships one named
		// "system", which the suite adopts. Other profiles create their own.
		if cfg.oscType == string(objectv1alpha1.ClusterTypeSystem) {
			cfg.oscName = systemOSCName
		} else {
			cfg.oscName = defaultOSCName
		}
	}
	if cfg.redundancy == "" {
		cfg.redundancy = defaultRedundancy
	}
	if cfg.oscSize == "" {
		cfg.oscSize = defaultOSCSize
	}
	if cfg.bucketName == "" {
		cfg.bucketName = defaultBucketName
	}
	if cfg.probeImage == "" {
		cfg.probeImage = defaultProbeImage
	}

	cfg.oscReadyTimeout = parseDuration(os.Getenv(envOSCReadyTimeout), defaultOSCReady)
	cfg.obReadyTimeout = parseDuration(os.Getenv(envOBReadyTimeout), defaultOBReady)
	cfg.moduleReadyTO = parseDuration(os.Getenv(envModuleReadyTO), defaultModuleReady)
	cfg.probeJobTimeout = parseDuration(os.Getenv(envProbeJobTimeout), defaultProbeJobTO)

	cfg.keepClusterOnFailure = envBool(os.Getenv(envKeepClusterOnFailure))
	cfg.skipSystemRecreate = envBool(os.Getenv(envSkipSystemRecreate))

	return cfg
}

// needsStorageClass reports whether the configured profile requires a real
// StorageClass (Lightweight/Full provision PVCs on spec.storage.class).
func (c e2eConfig) needsStorageClass() bool {
	return c.oscType == string(objectv1alpha1.ClusterTypeLightweight) ||
		c.oscType == string(objectv1alpha1.ClusterTypeFull)
}

func (c e2eConfig) isHeavy() bool {
	return c.oscType == string(objectv1alpha1.ClusterTypeHeavy)
}

func (c e2eConfig) isSystem() bool {
	return c.oscType == string(objectv1alpha1.ClusterTypeSystem)
}

// resolvePVCStorageClass picks the StorageClass for the PVC-backed profiles
// (Lightweight = Garage on PVC, Full = SeaweedFS on PVCs): E2E_PVC_STORAGE_CLASS,
// else E2E_STORAGE_CLASS, else the cluster's default StorageClass. Returns ""
// when none is available (the dependent specs then skip).
func resolvePVCStorageClass(ctx context.Context) (string, error) {
	if suiteCfg.pvcStorageClass != "" {
		return suiteCfg.pvcStorageClass, nil
	}
	if suiteCfg.storageCl != "" {
		return suiteCfg.storageCl, nil
	}
	scs, err := suiteClientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range scs.Items {
		if scs.Items[i].Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return scs.Items[i].Name, nil
		}
	}
	return "", nil
}

// groupVersionServed reports whether the apiserver serves the given
// "group/version" (used to gate the Full specs on the managed-postgres Postgres
// CRD being present).
func groupVersionServed(gv string) (bool, error) {
	_, err := suiteClientset.Discovery().ServerResourcesForGroupVersion(gv)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// envBool parses a permissive boolean env value ("true"/"1"/"yes", any case).
func envBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseDuration(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

func ensureNestedTestCluster() {
	if strings.TrimSpace(os.Getenv("TEST_CLUSTER_CREATE_MODE")) == "" {
		Fail("TEST_CLUSTER_CREATE_MODE must be set: this suite only supports storage-e2e nested clusters")
	}
	if suiteClusterResources != nil {
		return
	}
	suiteClusterResources = cluster.CreateOrConnectToTestCluster()
	if suiteClusterResources == nil || suiteClusterResources.Kubeconfig == nil {
		Fail("storage-e2e returned a nil cluster handle")
	}
}

func cleanupNestedTestCluster() {
	if suiteClusterResources == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := cluster.CleanupTestCluster(ctx, suiteClusterResources); err != nil {
		GinkgoWriter.Printf("  warning: nested cluster cleanup failed: %v\n", err)
	} else {
		GinkgoWriter.Printf("  nested cluster cleanup finished\n")
	}
	suiteClusterResources = nil
}

func ensureNamespace(ctx context.Context, name string) error {
	_, err := storagekube.CreateNamespaceIfNotExists(ctx, suiteRestCfg, name)
	return err
}

// waitModuleReady blocks until the sds-object Deckhouse module reports Ready.
func waitModuleReady(ctx context.Context) error {
	return storagekube.WaitForModuleReady(ctx, suiteRestCfg, moduleName, suiteCfg.moduleReadyTO)
}

// controllerDeploymentName is the sds-object controller Deployment in the module
// namespace. Its Pod runs both the reconciler and the "webhooks" container that
// backs the validating webhooks (webhooks.d8-sds-object.svc).
const controllerDeploymentName = "controller"

// waitControllerReady blocks until the sds-object controller Deployment has a
// Ready replica. The Deckhouse Module going Ready does not guarantee the
// controller Pod passed its readiness probe, so without this the first
// ObjectStore create can race the validating webhook and fail with
// "failed calling webhook ... connect: operation not permitted" (no ready
// endpoint behind the webhook Service yet).
func waitControllerReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		dep, err := suiteClientset.AppsV1().Deployments(moduleNS).Get(ctx, controllerDeploymentName, metav1.GetOptions{})
		if err == nil {
			if dep.Status.ReadyReplicas >= 1 && dep.Status.ReadyReplicas == dep.Status.Replicas {
				return nil
			}
			last = fmt.Sprintf("ready=%d/%d (updated=%d)", dep.Status.ReadyReplicas, dep.Status.Replicas, dep.Status.UpdatedReplicas)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for Deployment %s/%s to be Ready; last: %s", moduleNS, controllerDeploymentName, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// --- naming helpers ---------------------------------------------------------

// accessName is the BucketAccess name derived from a bucket name.
func accessName(bucket string) string { return bucket + "-access" }

// policyName is the BucketClaimPolicy name derived from a bucket name.
func policyName(bucket string) string { return bucket + "-policy" }

// claimName is the BucketClaim name derived from a bucket name.
func claimName(bucket string) string { return bucket + "-claim" }

// credsSecretName is the credentials Secret the access reconciler writes,
// defaulting to <access-name>-s3-credentials (unless spec.secretName overrides).
func credsSecretName(access string) string { return access + "-s3-credentials" }

// --- ObjectStore / Bucket / *Access / *Policy builders --

// buildOSC renders an ObjectStore from the suite config. storage and
// elasticClusterRef are only set for the profiles that accept them so the CRD's
// CEL "only allowed when ..." rules are satisfied.
func buildOSC(name string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"type": suiteCfg.oscType,
	}
	// System does not take its redundancy from E2E_REDUNDANCY: the profile only
	// accepts None, which selects the single-replica mode owned by the
	// sdsObject.systemBucket.singleReplica module setting (exercised by
	// systemSingleReplicaSpecs). Leaving it unset keeps a suite-created System
	// store on the default 3-replica profile the System specs assert.
	if !suiteCfg.isSystem() {
		spec["redundancy"] = suiteCfg.redundancy
	}
	if suiteCfg.needsStorageClass() {
		spec["storage"] = map[string]interface{}{
			"sizePerNode": suiteCfg.oscSize,
			"class":       suiteCfg.storageCl,
		}
	}
	if suiteCfg.isHeavy() {
		spec["elasticClusterRef"] = suiteCfg.elasticRef
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.ObjectStoreKind})
	u.SetName(name)
	u.Object["spec"] = spec
	return u
}

// buildOSB renders a cluster-scoped Bucket. The effective bucket
// name defaults to metadata.name (spec.bucketName is left unset here).
func buildOSB(name, objectStoreRef string, reclaim objectv1alpha1.BucketReclaimPolicy) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.BucketKind})
	u.SetName(name)
	u.Object["spec"] = map[string]interface{}{
		"objectStoreRef": objectStoreRef,
		"reclaimPolicy":  string(reclaim),
	}
	return u
}

// buildOSBFeatures renders a cluster-scoped Bucket like buildOSB but also sets
// the optional feature fields (accessPolicy and/or quota) so tests can assert
// how each backend reports them via the FeaturesApplied condition. An empty
// accessPolicy / nil quota leaves the field unset.
func buildOSBFeatures(name, objectStoreRef string, reclaim objectv1alpha1.BucketReclaimPolicy, accessPolicy objectv1alpha1.AccessPolicy, quota map[string]interface{}) *unstructured.Unstructured {
	u := buildOSB(name, objectStoreRef, reclaim)
	spec := u.Object["spec"].(map[string]interface{})
	if accessPolicy != "" {
		spec["accessPolicy"] = string(accessPolicy)
	}
	if quota != nil {
		spec["quota"] = quota
	}
	return u
}

// quotaEnforcedByBackend reports whether the backend behind the configured
// profile fully enforces the features spec's quota (both maxSize and
// maxObjects). Garage and Ceph RGW enforce both; SeaweedFS enforces only the
// size quota (no maxObjects), so with a quota that also sets maxObjects it
// reports FeaturesApplied=False. Kept in sync with the drivers' reporting.
func quotaEnforcedByBackend() bool {
	switch suiteCfg.oscType {
	case string(objectv1alpha1.ClusterTypeSystem),
		string(objectv1alpha1.ClusterTypeLightweight),
		string(objectv1alpha1.ClusterTypeHeavy):
		return true
	default: // Full → SeaweedFS enforces size only, not the maxObjects in the test quota
		return false
	}
}

// buildBucketClaim renders a namespaced brownfield BucketClaim that binds the
// existing Shared Bucket named existingBucketName. Binding is deny-by-default:
// a matching BucketClaimPolicy must allow ns before the claim reaches Bound.
func buildBucketClaim(name, ns, existingBucketName string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.BucketClaimKind})
	u.SetName(name)
	u.SetNamespace(ns)
	u.Object["spec"] = map[string]interface{}{
		"existingBucketName": existingBucketName,
	}
	return u
}

func createBucketClaim(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(bucketClaimGVR).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// buildGreenfieldClaim renders a namespaced greenfield BucketClaim: no
// existingBucketName, so the controller provisions a new private Bucket in
// objectStoreRef, owned by the claim (reserved-prefix name, origin=BucketClaim).
func buildGreenfieldClaim(name, ns, objectStoreRef string, reclaim objectv1alpha1.BucketReclaimPolicy) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.BucketClaimKind})
	u.SetName(name)
	u.SetNamespace(ns)
	u.Object["spec"] = map[string]interface{}{
		"objectStoreRef": objectStoreRef,
		"reclaimPolicy":  string(reclaim),
	}
	return u
}

var e2eReplicationFactorRE = regexp.MustCompile(`(?m)^replication_factor\s*=\s*(\d+)`)

// garageReplicationFactor reads the pinned replication_factor from the Garage
// garage.toml ConfigMap of the given ObjectStore (in the module namespace).
func garageReplicationFactor(ctx context.Context, storeName string) (int, error) {
	cm, err := suiteClientset.CoreV1().ConfigMaps(moduleNS).Get(ctx, storeName+"-garage-config", metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	m := e2eReplicationFactorRE.FindStringSubmatch(cm.Data["garage.toml"])
	if len(m) != 2 {
		return 0, fmt.Errorf("replication_factor not found in %s-garage-config", storeName)
	}
	return strconv.Atoi(m[1])
}

// systemIncarnationKey mirrors the controller's ConfigMap key recording the
// System data-plane incarnation (bumped on every recreate).
const systemIncarnationKey = "system-incarnation"

// garageSystemIncarnation reads the System data-plane incarnation from the same
// ConfigMap. A cluster that was never recreated reports 1 (the controller omits
// the key on clusters provisioned before the counter existed).
func garageSystemIncarnation(ctx context.Context, storeName string) (int, error) {
	cm, err := suiteClientset.CoreV1().ConfigMaps(moduleNS).Get(ctx, storeName+"-garage-config", metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	raw, ok := cm.Data[systemIncarnationKey]
	if !ok {
		return 1, nil
	}
	return strconv.Atoi(raw)
}

// systemLocalStorageClassName is the managed StorageClass backing the System
// profile's node-sticky local PVs.
const systemLocalStorageClassName = "sds-object-system-local"

// replicaBinding records how one System replica is nailed to its data: the PV its
// PVC is bound to and the node that PV's nodeAffinity pins. The PV UID is carried
// too, because pool PV names are deterministic per node and slot: a recycled
// replica can rebind the same NAME (a freshly created object) when there is only
// one master to place it on, so identity has to be compared by UID.
type replicaBinding struct {
	pvName string
	pvUID  types.UID
	node   string
}

// systemReplicaBindings maps each replica PVC of a System store to its current
// PV + pinned node. It is the observable form of the node-stickiness contract: the
// bindings must survive a pod restart untouched, and a recycled replica must show
// up with a different PV.
func systemReplicaBindings(ctx context.Context, storeName string) (map[string]replicaBinding, error) {
	pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(moduleNS).List(ctx, metav1.ListOptions{
		LabelSelector: "storage.deckhouse.io/object-store=" + storeName,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]replicaBinding, len(pvcs.Items))
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		binding := replicaBinding{pvName: pvc.Spec.VolumeName}
		if binding.pvName != "" {
			pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, binding.pvName, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			binding.pvUID = pv.UID
			binding.node = pvPinnedHostname(*pv)
		}
		out[pvc.Name] = binding
	}
	return out, nil
}

// garagePods lists the data-plane pods of a store.
func garagePods(ctx context.Context, storeName string) (*corev1.PodList, error) {
	return suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{
		LabelSelector: "storage.deckhouse.io/object-store=" + storeName,
	})
}

// deleteGaragePods deletes every data-plane pod of a store and returns the names
// (and UIDs) it deleted, so a spec can tell the replacements apart.
func deleteGaragePods(ctx context.Context, storeName string) (map[string]types.UID, error) {
	pods, err := garagePods(ctx, storeName)
	if err != nil {
		return nil, err
	}
	deleted := make(map[string]types.UID, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		deleted[p.Name] = p.UID
		if err := suiteClientset.CoreV1().Pods(moduleNS).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("delete pod %s: %w", p.Name, err)
		}
	}
	return deleted, nil
}

// statefulSetReadyReplicas returns the desired and ready replica counts of a
// StatefulSet in the module namespace, by its own name — the workloads are named
// per backend and per component (<store>-garage, <store>-seaweedfs-filer, …), so
// deriving the name here would only invite passing the wrong one.
func statefulSetReadyReplicas(ctx context.Context, name string) (desired, ready int32, err error) {
	sts, err := suiteClientset.AppsV1().StatefulSets(moduleNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, err
	}
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	return desired, sts.Status.ReadyReplicas, nil
}

// garageStatefulSetName is the data-plane workload of a Garage-backed store.
func garageStatefulSetName(storeName string) string { return storeName + "-garage" }

// execInPod runs cmd in a container of a pod and returns its stdout. Errors carry
// stderr, so a failing Garage CLI call is diagnosable from the spec output. Used to
// audit the backend's own state (keys, buckets), which no Kubernetes object
// reflects.
func execInPod(ctx context.Context, ns, pod, container string, cmd ...string) (string, error) {
	req := suiteClientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(suiteRestCfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("build executor for %s: %w", formatRef(ns, pod), err)
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return "", fmt.Errorf("exec %v in %s: %w (stderr: %s)", cmd, formatRef(ns, pod), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// garageConfigPath is where the module mounts garage.toml in the data-plane pods.
const garageConfigPath = "/etc/garage/garage.toml"

// garageCLI runs a `garage` subcommand inside a Running data-plane pod of the
// store. The binary talks to its local node over RPC using the mounted config
// (passed explicitly rather than relying on the env var) and the RPC secret
// already in the container's environment.
func garageCLI(ctx context.Context, storeName string, args ...string) (string, error) {
	pods, err := garagePods(ctx, storeName)
	if err != nil {
		return "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		cmd := append([]string{"/garage", "-c", garageConfigPath}, args...)
		return execInPod(ctx, moduleNS, p.Name, "garage", cmd...)
	}
	return "", fmt.Errorf("no Running Garage pod for ObjectStore %q", storeName)
}

// snapshotCredentials copies a credentials Secret into a plain Secret the module
// does not own, so a spec can keep using (or rather, try to use) the credentials
// after the controller has revoked the original.
func snapshotCredentials(ctx context.Context, ns, from, to string) error {
	src, err := suiteClientset.CoreV1().Secrets(ns).Get(ctx, from, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read credentials Secret %s: %w", formatRef(ns, from), err)
	}
	copied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: to, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       src.Data,
	}
	_ = suiteClientset.CoreV1().Secrets(ns).Delete(ctx, to, metav1.DeleteOptions{})
	if _, err := suiteClientset.CoreV1().Secrets(ns).Create(ctx, copied, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("copy credentials into %s: %w", formatRef(ns, to), err)
	}
	return nil
}

// containerLog fetches the (current instance's) log of one container in a pod —
// used to confirm the restore-node-key initContainer actually restored a persisted
// Garage identity.
func containerLog(ctx context.Context, ns, pod, container string) (string, error) {
	raw, err := suiteClientset.CoreV1().Pods(ns).
		GetLogs(pod, &corev1.PodLogOptions{Container: container}).
		DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s log of pod %s: %w", container, formatRef(ns, pod), err)
	}
	return string(raw), nil
}

// restartController deletes the module controller's pods and waits for the
// Deployment to be Ready again, so a spec can assert reconciliation resumes
// correctly after a restart in the middle of an operation.
func restartController(ctx context.Context, timeout time.Duration) error {
	// Take the selector from the Deployment rather than guessing its labels, which
	// are owned by the shared helm_lib controller template.
	dep, err := suiteClientset.AppsV1().Deployments(moduleNS).Get(ctx, controllerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get controller Deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return fmt.Errorf("build controller pod selector: %w", err)
	}
	pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no controller pods found in %s (label app=%s)", moduleNS, controllerDeploymentName)
	}
	for i := range pods.Items {
		name := pods.Items[i].Name
		if err := suiteClientset.CoreV1().Pods(moduleNS).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete controller pod %s: %w", name, err)
		}
	}
	return waitControllerReady(ctx, timeout)
}

// storeConditionMessage returns the message of one condition on an ObjectStore
// (empty when absent), for specs that watch the controller narrate progress.
func storeConditionMessage(ctx context.Context, name, condType string) string {
	u, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t != condType {
			continue
		}
		msg, _, _ := unstructured.NestedString(cm, "message")
		return msg
	}
	return ""
}

// moduleConfigGVR is the Deckhouse ModuleConfig resource (cluster-scoped); the
// suite patches spec.settings on it to drive module-level knobs the way an
// operator would.
var moduleConfigGVR = schema.GroupVersionResource{
	Group: "deckhouse.io", Version: "v1alpha1", Resource: "moduleconfigs",
}

// setSystemBucketSetting flips one boolean under sdsObject.systemBucket in the
// module's ModuleConfig, i.e. the real operator-facing path: Deckhouse re-renders
// the chart and the controller converges on the result.
func setSystemBucketSetting(ctx context.Context, key string, value bool) error {
	patch := fmt.Sprintf(`{"spec":{"settings":{"systemBucket":{%q:%t}}}}`, key, value)
	_, err := suiteDyn.Resource(moduleConfigGVR).Patch(ctx, moduleName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch ModuleConfig %s (%s=%t): %w", moduleName, key, value, err)
	}
	return nil
}

// setSystemSingleReplica switches the shipped system store between the default
// 3-replica profile and the single-replica one, which makes the controller recreate
// the data plane.
func setSystemSingleReplica(ctx context.Context, enabled bool) error {
	return setSystemBucketSetting(ctx, "singleReplica", enabled)
}

// setSystemBucketEnabled ships or unships the system ObjectStore, its bucket, the
// d8-* claim policy and the managed local StorageClass.
func setSystemBucketEnabled(ctx context.Context, enabled bool) error {
	return setSystemBucketSetting(ctx, "enabled", enabled)
}

// storeRedundancy returns spec.redundancy of an ObjectStore and whether the field
// is present at all (unset is meaningful: it is the System profile's default
// 3-replica mode).
func storeRedundancy(ctx context.Context, name string) (string, bool, error) {
	u, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", false, err
	}
	value, found, err := unstructured.NestedString(u.Object, "spec", "redundancy")
	return value, found, err
}

// buildOSBPolicy renders a cluster-scoped BucketClaimPolicy that allows
// the given namespaces (by exact name) to request access to bucketRef. Access is
// deny-by-default, so a matching policy must exist before an
// BucketAccess in one of these namespaces can reach Ready.
func buildOSBPolicy(name, bucketRef string, namespaces []string) *unstructured.Unstructured {
	names := make([]interface{}, 0, len(namespaces))
	for _, n := range namespaces {
		names = append(names, n)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.BucketClaimPolicyKind})
	u.SetName(name)
	u.Object["spec"] = map[string]interface{}{
		"bucketRef": bucketRef,
		"allowedNamespaces": map[string]interface{}{
			"names": names,
		},
	}
	return u
}

// buildOSBAccess renders a namespaced BucketAccess referencing the BucketClaim
// bucketClaimName in the same namespace. The controller writes the credentials
// Secret in ns (named <name>-s3-credentials) owned by this access.
func buildOSBAccess(name, ns, bucketClaimName string, permission objectv1alpha1.AccessPermission) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"bucketClaimName": bucketClaimName,
	}
	if permission != "" {
		spec["permission"] = string(permission)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.BucketAccessKind})
	u.SetName(name)
	u.SetNamespace(ns)
	u.Object["spec"] = spec
	return u
}

func createOSC(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(objectStoreGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// oscExists reports whether the named (cluster-scoped) ObjectStore
// already exists — used to adopt the module-shipped `system` cluster instead of
// creating a second one.
func oscExists(ctx context.Context, name string) (bool, error) {
	_, err := suiteDyn.Resource(objectStoreGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func createOSB(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(bucketGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

func createOSBPolicy(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(bucketClaimPolicyGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// ensureOSBPolicy creates the policy unless it is already there. Policies are not
// tied to a Bucket's lifecycle, so a spec that re-declares a bucket may find the
// policy from an earlier spec still in place — and failing on AlreadyExists would
// report a fixture collision as a product failure.
func ensureOSBPolicy(ctx context.Context, u *unstructured.Unstructured) error {
	err := createOSBPolicy(ctx, u)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func createOSBAccess(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(bucketAccessGVR).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
	return err
}

// --- status / condition readers --------------------------------------------

// getCondition returns (status, reason, found) for a condition type on the
// status.conditions[] of a dynamic object. ns="" addresses cluster-scoped.
func getCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condType string) (status, reason string, found bool, err error) {
	var obj *unstructured.Unstructured
	if ns == "" {
		obj, err = suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return "", "", false, err
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t == condType {
			st, _, _ := unstructured.NestedString(cm, "status")
			rs, _, _ := unstructured.NestedString(cm, "reason")
			return st, rs, true, nil
		}
	}
	return "", "", false, nil
}

func waitCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condType, wantStatus string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		status, reason, found, err := getCondition(ctx, gvr, ns, name, condType)
		if err == nil && found && status == wantStatus {
			return nil
		}
		last = fmt.Sprintf("found=%v status=%q reason=%q err=%v", found, status, reason, err)
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s %s condition %s=%s; last: %s", gvr.Resource, formatRef(ns, name), condType, wantStatus, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

func waitOSCReady(ctx context.Context, name string) error {
	return waitCondition(ctx, objectStoreGVR, "", name, objectv1alpha1.ObjectStoreConditionReady, "True", suiteCfg.oscReadyTimeout)
}

// waitOSBReady blocks until the cluster-scoped Bucket is Ready.
func waitOSBReady(ctx context.Context, name string) error {
	return waitCondition(ctx, bucketGVR, "", name, objectv1alpha1.BucketConditionReady, "True", suiteCfg.obReadyTimeout)
}

// waitAccessReady blocks until the namespaced BucketAccess is Ready
// (which requires a matching BucketClaimPolicy for its bucket).
func waitAccessReady(ctx context.Context, ns, name string) error {
	return waitCondition(ctx, bucketAccessGVR, ns, name, objectv1alpha1.BucketAccessConditionReady, "True", suiteCfg.obReadyTimeout)
}

// getStringField fetches a nested string field from a dynamic object.
func getStringField(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, fields ...string) (string, error) {
	var obj *unstructured.Unstructured
	var err error
	if ns == "" {
		obj, err = suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return "", err
	}
	val, _, _ := unstructured.NestedString(obj.Object, fields...)
	return val, nil
}

// waitResourceGone blocks until a dynamic GET of the resource returns NotFound.
// ns="" addresses cluster-scoped resources.
func waitResourceGone(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var err error
		if ns == "" {
			_, err = suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		} else {
			_, err = suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		}
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timeout waiting for %s %s to be gone; last get err: %w", gvr.Resource, formatRef(ns, name), err)
			}
			return fmt.Errorf("timeout waiting for %s %s to be gone (still present)", gvr.Resource, formatRef(ns, name))
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

func waitSecretGone(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := suiteClientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for Secret %s to be gone; last get err: %v", formatRef(ns, name), err)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// --- S3 round-trip probe Job -----------------------------------------------

// runS3ProbeJob creates a one-shot Job that writes, lists and reads back an
// object via `mc`, consuming the bucket credentials Secret with envFrom, and
// waits for it to succeed. The Job body mirrors testing/*.yaml.
func runS3ProbeJob(ctx context.Context, jobName, ns, secretName string) error {
	script := strings.Join([]string{
		s3AliasLine(),
		fmt.Sprintf("echo \"hello from sds-object e2e\" | mc pipe \"%s/$%s/hello.txt\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		fmt.Sprintf("echo '--- listing ---'; mc ls \"%s/$%s\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		fmt.Sprintf("got=$(mc cat \"%s/$%s/hello.txt\")", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"echo \"--- content: $got ---\"",
		"test \"$got\" = \"hello from sds-object e2e\"",
		"echo S3 OK",
	}, "\n")

	return runS3ScriptJob(ctx, jobName, ns, secretName, 10, script)
}

// s3AliasLine is the shell prologue every probe script shares: fail on the first
// error and register the `mc` alias from the credentials Secret's env.
func s3AliasLine() string {
	return "set -e\n" + fmt.Sprintf("mc alias set %s \"$%s\" \"$%s\" \"$%s\"",
		probeAlias, objectv1alpha1.SecretKeyS3Endpoint, objectv1alpha1.SecretKeyAccessKeyID, objectv1alpha1.SecretKeySecretAccessID)
}

// s3PutMarker writes a marker object with known content into the bucket, so a
// later spec can prove whether the stored data survived an operation.
func s3PutMarker(ctx context.Context, jobName, ns, secretName, object, content string) error {
	script := strings.Join([]string{
		s3AliasLine(),
		fmt.Sprintf("printf '%%s' %q | mc pipe \"%s/$%s/%s\"", content, probeAlias, objectv1alpha1.SecretKeyS3Bucket, object),
		fmt.Sprintf("got=$(mc cat \"%s/$%s/%s\")", probeAlias, objectv1alpha1.SecretKeyS3Bucket, object),
		fmt.Sprintf("test \"$got\" = %q", content),
		"echo MARKER WRITTEN",
	}, "\n")

	return runS3ScriptJob(ctx, jobName, ns, secretName, 10, script)
}

// s3AssertMarkerContent succeeds only when the marker object is still there with
// exactly the content it was written with — the observable form of "the data
// survived".
func s3AssertMarkerContent(ctx context.Context, jobName, ns, secretName, object, content string) error {
	script := strings.Join([]string{
		s3AliasLine(),
		fmt.Sprintf("echo '--- listing ---'; mc ls \"%s/$%s\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		fmt.Sprintf("got=$(mc cat \"%s/$%s/%s\")", probeAlias, objectv1alpha1.SecretKeyS3Bucket, object),
		"echo \"--- content: $got ---\"",
		fmt.Sprintf("test \"$got\" = %q", content),
		"echo MARKER INTACT",
	}, "\n")

	return runS3ScriptJob(ctx, jobName, ns, secretName, 10, script)
}

// s3AssertQuotaEnforced fills a bucket up to its object quota and then keeps
// trying to exceed it until a write is refused. Polling matters: the backend
// counts objects asynchronously, so the first write past the limit can still be
// accepted — a single attempt would be flaky. Once a write is refused, a read is
// done to prove the client and credentials are fine and the refusal was specific
// to the quota rather than a broken connection.
func s3AssertQuotaEnforced(ctx context.Context, jobName, ns, secretName string, maxObjects int) error {
	lines := []string{
		s3AliasLine(),
		fmt.Sprintf("i=0; while [ $i -lt %d ]; do i=$((i+1)); echo filler | mc pipe \"%s/$%s/quota-fill-$i.txt\"; done",
			maxObjects, probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"echo '--- at quota, trying to exceed it ---'",
		"refused=0; attempt=0",
		"while [ $attempt -lt 30 ]; do",
		"  attempt=$((attempt+1))",
		fmt.Sprintf("  if echo over | mc pipe \"%s/$%s/quota-over-$attempt.txt\" 2>/tmp/err; then", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"    sleep 2; continue",
		"  fi",
		"  refused=1; echo \"--- refusal after $attempt attempt(s): $(cat /tmp/err) ---\"; break",
		"done",
		"if [ \"$refused\" != 1 ]; then",
		"  echo \"ERROR: the object quota was never enforced\"",
		"  exit 1",
		"fi",
		fmt.Sprintf("mc cat \"%s/$%s/quota-fill-1.txt\" >/dev/null", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"echo QUOTA ENFORCED",
	}

	return runS3ScriptJob(ctx, jobName, ns, secretName, 3, strings.Join(lines, "\n"))
}

// s3AssertSizeQuotaEnforced writes one small object (which must succeed, proving
// the credentials and bucket are fine) and then keeps writing an object larger than
// the whole quota until a write is refused. It builds its payload by doubling a
// shell string, so it needs no bulk-data tooling in the probe image, and it polls
// for the same reason as the object-count variant: backends account usage
// asynchronously.
func s3AssertSizeQuotaEnforced(ctx context.Context, jobName, ns, secretName string) error {
	lines := []string{
		s3AliasLine(),
		fmt.Sprintf("printf ok | mc pipe \"%s/$%s/size-under.bin\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"payload=0123456789abcdef",
		"i=0; while [ $i -lt 8 ]; do payload=\"$payload$payload\"; i=$((i+1)); done", // 16 * 2^8 = 4096 bytes
		"echo '--- payload is 4Ki, quota is 1Ki ---'",
		"refused=0; attempt=0",
		"while [ $attempt -lt 30 ]; do",
		"  attempt=$((attempt+1))",
		fmt.Sprintf("  if printf '%%s' \"$payload\" | mc pipe \"%s/$%s/size-over-$attempt.bin\" 2>/tmp/err; then", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"    sleep 2; continue",
		"  fi",
		"  refused=1; echo \"--- refusal after $attempt attempt(s): $(cat /tmp/err) ---\"; break",
		"done",
		"if [ \"$refused\" != 1 ]; then",
		"  echo \"ERROR: the size quota was never enforced\"",
		"  exit 1",
		"fi",
		"echo SIZE QUOTA ENFORCED",
	}

	return runS3ScriptJob(ctx, jobName, ns, secretName, 3, strings.Join(lines, "\n"))
}

// s3AssertCredentialsRejected succeeds only when the given credentials are
// REFUSED by the backend. Revocation asserted at the Kubernetes level (the Secret
// is gone, the condition is False) says nothing about the backend still honouring
// the key, which is the half that matters; this closes it.
//
// The alias is configured through MC_HOST_<alias> rather than `mc alias set`,
// which validates the credentials up front — a failure there would abort the
// script before it could tell "rejected" from "misconfigured".
//
// It does not try to recognise the refusal by wording: `mc` renders each backend's
// auth error in its own words (Garage's InvalidAccessKeyId arrives as "The Access
// Key Id you provided does not exist in our records"), so a whitelist of phrases
// only produces false failures. Instead the transport-level failures that would
// prove nothing about revocation are rejected explicitly, and callers pair this
// with expectBackendKeyGone for an authoritative check.
func s3AssertCredentialsRejected(ctx context.Context, jobName, ns, secretName string) error {
	script := strings.Join([]string{
		"set -e",
		fmt.Sprintf("host=${%s#*://}", objectv1alpha1.SecretKeyS3Endpoint),
		fmt.Sprintf("export MC_HOST_%s=\"http://$%s:$%s@$host\"",
			probeAlias, objectv1alpha1.SecretKeyAccessKeyID, objectv1alpha1.SecretKeySecretAccessID),
		fmt.Sprintf("if out=$(mc ls \"%s/$%s\" 2>&1); then", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"  echo \"ERROR: the revoked credentials still work: $out\"",
		"  exit 1",
		"fi",
		"echo \"--- refusal: $out ---\"",
		"case \"$out\" in",
		"  *\"no such host\"*|*\"connection refused\"*|*\"i/o timeout\"*|*\"context deadline\"*|*\"EOF\"*)",
		"    echo \"ERROR: the endpoint was unreachable, which proves nothing about revocation\"",
		"    exit 1;;",
		"esac",
		"echo CREDENTIALS REJECTED",
	}, "\n")

	// A revoked key must be refused; two attempts tolerate one transient hiccup
	// without masking a backend that keeps honouring the key.
	return runS3ScriptJob(ctx, jobName, ns, secretName, 2, script)
}

// s3AssertMarkerAbsent succeeds only when the credentials work, the bucket is
// listable AND the marker object is gone. Listing first is what makes the
// assertion trustworthy: a job that merely failed to authenticate (e.g. the key
// has not been re-issued yet) errors out and is retried instead of being read as
// "the object is gone".
func s3AssertMarkerAbsent(ctx context.Context, jobName, ns, secretName, object string) error {
	script := strings.Join([]string{
		s3AliasLine(),
		fmt.Sprintf("echo '--- listing ---'; mc ls \"%s/$%s\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		fmt.Sprintf("if mc stat \"%s/$%s/%s\" >/dev/null 2>&1; then", probeAlias, objectv1alpha1.SecretKeyS3Bucket, object),
		fmt.Sprintf("  echo \"ERROR: %s still exists; the store was NOT recreated from scratch\"", object),
		"  exit 1",
		"fi",
		"echo MARKER GONE",
	}, "\n")

	// A low backoff keeps a genuine "the object survived" failure fast, while
	// still tolerating a few retries for a bucket or key that is mid-reprovision.
	return runS3ScriptJob(ctx, jobName, ns, secretName, 4, script)
}

// runS3ScriptJob runs an arbitrary `mc` script as a one-shot Job with the bucket
// credentials Secret in its environment and waits for it to succeed.
func runS3ScriptJob(ctx context.Context, jobName, ns, secretName string, backoffLimit int32, script string) error {
	backoff := backoffLimit
	var ttl int32 = 600
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "mc",
						Image:   suiteCfg.probeImage,
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{script},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
							},
						}},
					}},
				},
			},
		},
	}

	// Replace any leftover Job from a previous run.
	_ = suiteClientset.BatchV1().Jobs(ns).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationForeground)})
	if _, err := suiteClientset.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create probe job %s: %w", formatRef(ns, jobName), err)
	}

	deadline := time.Now().Add(suiteCfg.probeJobTimeout)
	for {
		j, err := suiteClientset.BatchV1().Jobs(ns).Get(ctx, jobName, metav1.GetOptions{})
		if err == nil {
			if j.Status.Succeeded > 0 {
				return nil
			}
			if j.Status.Failed >= backoff {
				dumpJobLogs(ctx, ns, jobName)
				return fmt.Errorf("probe job %s failed (%d attempts); see its log above", formatRef(ns, jobName), j.Status.Failed)
			}
		}
		if time.Now().After(deadline) {
			dumpJobLogs(ctx, ns, jobName)
			s, f := jobStatus(j)
			return fmt.Errorf("timeout waiting for probe job %s to succeed (succeeded=%d failed=%d)", formatRef(ns, jobName), s, f)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// dumpJobLogs prints the logs of a probe Job's pods. Every probe script narrates
// what it saw before failing, and that narration is the whole diagnosis — without
// it a failure only says "the job exited non-zero", which is unreadable in CI where
// the cluster is gone by the time anyone looks.
func dumpJobLogs(ctx context.Context, ns, jobName string) {
	pods, err := suiteClientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		GinkgoWriter.Printf("  probe job %s: cannot list pods: %v\n", formatRef(ns, jobName), err)
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		log, err := containerLog(ctx, ns, p.Name, "mc")
		if err != nil {
			GinkgoWriter.Printf("  probe pod %s (%s): cannot read log: %v\n", p.Name, p.Status.Phase, err)
			continue
		}
		GinkgoWriter.Printf("  probe pod %s (%s) log:\n%s\n", p.Name, p.Status.Phase, strings.TrimRight(log, "\n"))
	}
}

func jobStatus(j *batchv1.Job) (int32, int32) {
	if j == nil {
		return 0, 0
	}
	return j.Status.Succeeded, j.Status.Failed
}

func formatRef(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func ptr[T any](v T) *T { return &v }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
