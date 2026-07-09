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

package cephrgw

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
)

// elasticNamespace is where sds-elastic runs Rook and the CephCluster, and
// where the CephObjectStore must be created (Rook attaches it to the
// CephCluster in the same namespace).
const elasticNamespace = "d8-sds-elastic"

// s3Region advertised for the RGW endpoint.
const s3Region = "us-east-1"

// metadataPoolReplicas mirrors sds-elastic's metadata pool size.
const metadataPoolReplicas = 3

// rgwPort is the default Rook RGW HTTP port.
const rgwPort = 80

// GroupVersionKinds. sds-elastic vendors Rook under a renamed API group
// (internal.sdselastic.deckhouse.io) so it does not clash with an upstream
// Rook; we address that group. ElasticCluster is sds-elastic's own CR.
var (
	cephObjectStoreGVK = schema.GroupVersionKind{
		Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephObjectStore",
	}
	cephObjectStoreUserGVK = schema.GroupVersionKind{
		Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephObjectStoreUser",
	}
	elasticClusterGVK = schema.GroupVersionKind{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "ElasticCluster",
	}
)

// storeName is the CephObjectStore name (in d8-sds-elastic) for a cluster.
func storeName(cluster *v1alpha1.ObjectStore) string {
	return cluster.Name
}

func commonLabels(cluster *v1alpha1.ObjectStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":      "sds-object",
		"app.kubernetes.io/name":            "ceph-rgw",
		"storage.deckhouse.io/object-store": cluster.Name,
	}
}

// rgwEndpoint is the in-cluster S3 URL of the Rook RGW Service.
func rgwEndpoint(cluster *v1alpha1.ObjectStore, clusterDomain string) string {
	return fmt.Sprintf("http://rook-ceph-rgw-%s.%s.svc.%s:%d", storeName(cluster), elasticNamespace, clusterDomain, rgwPort)
}

// rgwHostPort is the host:port of the RGW endpoint (no scheme), for the S3 client.
func rgwHostPort(cluster *v1alpha1.ObjectStore, clusterDomain string) string {
	return fmt.Sprintf("rook-ceph-rgw-%s.%s.svc.%s:%d", storeName(cluster), elasticNamespace, clusterDomain, rgwPort)
}

// ownerUID is the RGW user id (and CephObjectStoreUser CR name) of the
// per-bucket owner user that creates and owns the bucket.
func ownerUID(bucket *v1alpha1.Bucket) string {
	return backend.BucketDisplayName(bucket) + "-owner"
}

// accessUID is the RGW user id (and CephObjectStoreUser CR name) issued for an
// BucketAccess.
func accessUID(access *v1alpha1.BucketAccess) string {
	return backend.AccessResourceName(access)
}

// rgwUserSecretName is the Secret Rook generates for a CephObjectStoreUser with
// the given uid, in elasticNamespace (data keys AccessKey/SecretKey/Endpoint).
func rgwUserSecretName(cluster *v1alpha1.ObjectStore, uid string) string {
	return fmt.Sprintf("rook-ceph-object-user-%s-%s", storeName(cluster), uid)
}

// buildCephObjectStoreUser returns the Rook CephObjectStoreUser with the given
// RGW user id.
func buildCephObjectStoreUser(cluster *v1alpha1.ObjectStore, uid string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cephObjectStoreUserGVK)
	obj.SetName(uid)
	obj.SetNamespace(elasticNamespace)
	obj.SetLabels(commonLabels(cluster))
	obj.Object["spec"] = map[string]interface{}{
		"store":       storeName(cluster),
		"displayName": uid,
	}
	return obj
}

// replicatedPool maps the redundancy intent to a Ceph replicated pool spec,
// mirroring sds-elastic's ElasticStorageClass replication conventions.
func replicatedPool(cluster *v1alpha1.ObjectStore) map[string]interface{} {
	size := int64(3)
	safe := true
	switch cluster.Spec.Redundancy {
	case v1alpha1.RedundancyNone:
		// Ceph size=1 is unsafe/impractical (any OSD loss loses data and blocks
		// I/O), so the minimum for the Single intent is 2 copies with the safe-
		// replica-size guard disabled (mirrors sds-elastic's convention).
		size, safe = 2, false
	case v1alpha1.RedundancyHigh:
		size = 4
	}
	return map[string]interface{}{
		"size":                   size,
		"requireSafeReplicaSize": safe,
	}
}

// buildCephObjectStore returns the Rook CephObjectStore for the cluster.
func buildCephObjectStore(cluster *v1alpha1.ObjectStore) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cephObjectStoreGVK)
	obj.SetName(storeName(cluster))
	obj.SetNamespace(elasticNamespace)
	obj.SetLabels(commonLabels(cluster))
	obj.Object["spec"] = map[string]interface{}{
		"metadataPool": map[string]interface{}{
			"failureDomain": "host",
			"replicated": map[string]interface{}{
				"size":                   int64(metadataPoolReplicas),
				"requireSafeReplicaSize": true,
			},
		},
		"dataPool": map[string]interface{}{
			"failureDomain": "host",
			"replicated":    replicatedPool(cluster),
		},
		// Preserve the RGW data/metadata pools unless the cluster is explicitly
		// reclaimed with Delete. Otherwise deleting the ObjectStore (or
		// even a Rook-side CephObjectStore churn) would destroy all bucket data
		// regardless of any bucket's Retain policy.
		"preservePoolsOnDelete": cluster.Spec.ReclaimPolicy != v1alpha1.ClusterReclaimDelete,
		"gateway": map[string]interface{}{
			"port":      int64(rgwPort),
			"instances": int64(1),
		},
	}
	return obj
}

// objectStoreKey is the lookup key for the CephObjectStore.
func objectStoreKey(cluster *v1alpha1.ObjectStore) (namespace, name string) {
	return elasticNamespace, storeName(cluster)
}

// newUnstructured returns an empty object of the given GVK for Get calls.
func newUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}
