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
	"context"
	"fmt"
	"os"
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
	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

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
)

const (
	defaultOSCName     = "e2e-osc"
	defaultOSCType     = string(objectv1alpha1.ClusterTypeSystem)
	defaultRedundancy  = string(objectv1alpha1.RedundancySingle)
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
	objectStorageClusterGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "objectstorageclusters",
	}
	objectBucketGVR = schema.GroupVersionResource{
		Group: apiGroup, Version: apiVersion, Resource: "objectbuckets",
	}

	// credsSecretKeys are the standardised keys the bucket reconciler writes into
	// the credentials Secret (status.secretRef). The suite asserts all are present
	// and non-empty, and the probe Job envFroms the Secret directly.
	credsSecretKeys = []string{
		objectv1alpha1.SecretKeyS3Endpoint,
		objectv1alpha1.SecretKeyS3Region,
		objectv1alpha1.SecretKeyS3Bucket,
		objectv1alpha1.SecretKeyAccessKeyID,
		objectv1alpha1.SecretKeySecretAccessID,
	}
)

type e2eConfig struct {
	// namespace is the in-cluster namespace for ObjectBuckets/Secrets/probe Pods.
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
}

var (
	suiteCfg              e2eConfig
	suiteRestCfg          *rest.Config
	suiteClientset        *clientgokube.Clientset
	suiteDyn              dynamic.Interface
	suiteClusterResources *cluster.TestClusterResources
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
	if cfg.oscName == "" {
		cfg.oscName = defaultOSCName
	}
	if cfg.oscType == "" {
		cfg.oscType = defaultOSCType
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
// ObjectStorageCluster create can race the validating webhook and fail with
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

// --- ObjectStorageCluster / ObjectBucket builders --------------------------

// buildOSC renders an ObjectStorageCluster from the suite config. storage and
// elasticClusterRef are only set for the profiles that accept them so the CRD's
// CEL "only allowed when ..." rules are satisfied.
func buildOSC(name string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"type":       suiteCfg.oscType,
		"redundancy": suiteCfg.redundancy,
	}
	if suiteCfg.needsStorageClass() {
		spec["storage"] = map[string]interface{}{
			"size":  suiteCfg.oscSize,
			"class": suiteCfg.storageCl,
		}
	}
	if suiteCfg.isHeavy() {
		spec["elasticClusterRef"] = suiteCfg.elasticRef
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.ObjectStorageClusterKind})
	u.SetName(name)
	u.Object["spec"] = spec
	return u
}

func buildOB(name, ns, clusterRef string, reclaim objectv1alpha1.BucketReclaimPolicy) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: apiGroup, Version: apiVersion, Kind: objectv1alpha1.ObjectBucketKind})
	u.SetName(name)
	u.SetNamespace(ns)
	u.Object["spec"] = map[string]interface{}{
		"clusterRef":    clusterRef,
		"reclaimPolicy": string(reclaim),
	}
	return u
}

func createOSC(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(objectStorageClusterGVR).Create(ctx, u, metav1.CreateOptions{})
	return err
}

func createOB(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := suiteDyn.Resource(objectBucketGVR).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
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
	return waitCondition(ctx, objectStorageClusterGVR, "", name, objectv1alpha1.OSCConditionReady, "True", suiteCfg.oscReadyTimeout)
}

func waitOBReady(ctx context.Context, ns, name string) error {
	return waitCondition(ctx, objectBucketGVR, ns, name, objectv1alpha1.OBConditionReady, "True", suiteCfg.obReadyTimeout)
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
		"set -e",
		fmt.Sprintf("mc alias set %s \"$%s\" \"$%s\" \"$%s\"", probeAlias, objectv1alpha1.SecretKeyS3Endpoint, objectv1alpha1.SecretKeyAccessKeyID, objectv1alpha1.SecretKeySecretAccessID),
		fmt.Sprintf("echo \"hello from sds-object e2e\" | mc pipe \"%s/$%s/hello.txt\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		fmt.Sprintf("echo '--- listing ---'; mc ls \"%s/$%s\"", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		fmt.Sprintf("got=$(mc cat \"%s/$%s/hello.txt\")", probeAlias, objectv1alpha1.SecretKeyS3Bucket),
		"echo \"--- content: $got ---\"",
		"test \"$got\" = \"hello from sds-object e2e\"",
		"echo S3 OK",
	}, "\n")

	backoff := int32(10)
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
				return fmt.Errorf("probe job %s failed (%d attempts); inspect `kubectl -n %s logs job/%s`", formatRef(ns, jobName), j.Status.Failed, ns, jobName)
			}
		}
		if time.Now().After(deadline) {
			s, f := jobStatus(j)
			return fmt.Errorf("timeout waiting for probe job %s to succeed (succeeded=%d failed=%d)", formatRef(ns, jobName), s, f)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
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
