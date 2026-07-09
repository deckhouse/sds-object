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

package garage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// Port numbers used by the Garage processes.
const (
	s3Port    = 3900
	rpcPort   = 3901
	adminPort = 3903
)

// Fixed paths inside the Garage container.
const (
	dataMountPath  = "/var/lib/garage"
	configMount    = "/etc/garage"
	configFileName = "garage.toml"

	// hostPathBase is where the System profile keeps Garage data on
	// control-plane nodes.
	hostPathBase = "/var/lib/deckhouse/sds-object/garage"

	registryPullSecret = "deckhouse-registry"

	// controlPlaneNodeLabel marks control-plane nodes; the System profile
	// schedules its DaemonSet there and the initial replication factor is clamped
	// to the count of nodes carrying it.
	controlPlaneNodeLabel = "node-role.kubernetes.io/control-plane"

	// annConfigHash carries a short hash of garage.toml on the pod template so
	// the workload rolls (and every node converges to the same config) when the
	// config changes.
	annConfigHash = "storage.deckhouse.io/config-hash"
)

// resourceName is the common name/prefix for every object backing a cluster.
func resourceName(cluster *v1alpha1.ObjectStore) string {
	return cluster.Name + "-garage"
}

func secretName(cluster *v1alpha1.ObjectStore) string {
	return resourceName(cluster) + "-secrets"
}
func configName(cluster *v1alpha1.ObjectStore) string {
	return resourceName(cluster) + "-config"
}
func rpcSvcName(cluster *v1alpha1.ObjectStore) string { return resourceName(cluster) + "-rpc" }
func s3SvcName(cluster *v1alpha1.ObjectStore) string  { return resourceName(cluster) }

// commonLabels are placed on every object owned by a cluster.
func commonLabels(cluster *v1alpha1.ObjectStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":      "sds-object",
		"app.kubernetes.io/name":            "garage",
		"storage.deckhouse.io/object-store": cluster.Name,
	}
}

// s3Endpoint is the in-cluster S3 URL of the cluster's Service.
func s3Endpoint(cluster *v1alpha1.ObjectStore, namespace, clusterDomain string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", s3SvcName(cluster), namespace, clusterDomain, s3Port)
}

// s3HostPort is the in-cluster S3 endpoint as host:port (no scheme), for the
// minio/S3 client used to empty buckets before deletion.
func s3HostPort(cluster *v1alpha1.ObjectStore, namespace, clusterDomain string) string {
	return fmt.Sprintf("%s.%s.svc.%s:%d", s3SvcName(cluster), namespace, clusterDomain, s3Port)
}

// adminEndpoint is the in-cluster admin API URL of the cluster's Service.
func adminEndpoint(cluster *v1alpha1.ObjectStore, namespace, clusterDomain string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", s3SvcName(cluster), namespace, clusterDomain, adminPort)
}

// replicationFactor maps the high-level redundancy intent to the desired Garage
// replication_factor: None=1, Standard (default)=3, High=5. This is only the
// initial intent used at cluster creation; the effective factor is clamped to
// the node count and then PINNED for the cluster's lifetime (see
// Driver.pinnedReplicationFactor). Garage does not support changing
// replication_factor on a live cluster, so it must never be recomputed from the
// current node count afterwards.
func replicationFactor(cluster *v1alpha1.ObjectStore) int32 {
	switch cluster.Spec.Redundancy {
	case v1alpha1.RedundancyNone:
		return 1
	case v1alpha1.RedundancyHigh:
		return 5
	default: // RedundancyStandard or unset
		return 3
	}
}

// clampRF caps desired at the available node count, with a floor of 1. Garage
// accepts any replication factor in [1, nodeCount] (2 is a valid, supported
// mode: tolerates one node down, read-only while degraded), so no odd rounding
// is applied. E.g. clampRF(3, 1)=1, clampRF(3, 2)=2, clampRF(5, 4)=4.
func clampRF(desired, nodes int32) int32 {
	rf := desired
	if nodes < rf {
		rf = nodes
	}
	if rf < 1 {
		rf = 1
	}
	return rf
}

// replicationFactorRE extracts replication_factor from an existing garage.toml,
// so a running cluster's (pinned) factor can be read back rather than
// recomputed.
var replicationFactorRE = regexp.MustCompile(`(?m)^replication_factor\s*=\s*(\d+)`)

// replicationFactorFromConfigMap returns the replication_factor recorded in an
// existing garage.toml ConfigMap, or 0 when the ConfigMap is nil or the value is
// absent/unparseable.
func replicationFactorFromConfigMap(cm *corev1.ConfigMap) int32 {
	if cm == nil {
		return 0
	}
	m := replicationFactorRE.FindStringSubmatch(cm.Data[configFileName])
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0
	}
	return int32(n)
}

// configHash is a short content hash of garage.toml, stamped onto the pod
// template so the workload rolls when the config changes (Garage reads the file
// only at startup, and every node must run the same replication_factor).
func configHash(cfg string) string {
	sum := sha256.Sum256([]byte(cfg))
	return hex.EncodeToString(sum[:])[:16]
}

// lightweightReplicas is the StatefulSet replica count for the Lightweight
// profile: spec.storage.nodes when set, otherwise the redundancy-derived
// replication factor.
func lightweightReplicas(cluster *v1alpha1.ObjectStore) int32 {
	if cluster.Spec.Storage != nil && cluster.Spec.Storage.Nodes != nil && *cluster.Spec.Storage.Nodes >= 1 {
		return *cluster.Spec.Storage.Nodes
	}
	return replicationFactor(cluster)
}

// storageSize returns the per-node PVC size for PVC-backed profiles, defaulting
// to 10Gi when spec.storage.sizePerNode is unset/invalid.
func storageSize(cluster *v1alpha1.ObjectStore) resource.Quantity {
	if cluster.Spec.Storage != nil && cluster.Spec.Storage.SizePerNode != "" {
		if q, err := resource.ParseQuantity(cluster.Spec.Storage.SizePerNode); err == nil {
			return q
		}
	}
	return resource.MustParse("10Gi")
}

// renderConfig produces the garage.toml content for the given (already clamped)
// replication factor. Secrets (rpc, admin token) are supplied via environment
// variables, not the file.
func renderConfig(rf int32) string {
	return fmt.Sprintf(`metadata_dir = "%s/meta"
data_dir = "%s/data"
db_engine = "lmdb"

replication_factor = %d

rpc_bind_addr = "[::]:%d"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:%d"
root_domain = ".s3.garage"

[admin]
api_bind_addr = "[::]:%d"
`, dataMountPath, dataMountPath, rf, rpcPort, s3Port, adminPort)
}

// buildConfigMap returns the ConfigMap holding garage.toml with the given
// replication factor.
func buildConfigMap(cluster *v1alpha1.ObjectStore, namespace string, rf int32) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Data: map[string]string{configFileName: renderConfig(rf)},
	}
}

// buildServiceAccount returns the ServiceAccount the Garage pods run as.
func buildServiceAccount(cluster *v1alpha1.ObjectStore, namespace string) *corev1.ServiceAccount {
	automount := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		AutomountServiceAccountToken: &automount,
	}
}

// buildS3Service returns the ClusterIP Service exposing the S3 and admin APIs.
func buildS3Service(cluster *v1alpha1.ObjectStore, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s3SvcName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Spec: corev1.ServiceSpec{
			Selector: commonLabels(cluster),
			Ports: []corev1.ServicePort{
				{Name: "s3", Port: s3Port, TargetPort: intstr.FromInt(s3Port)},
				{Name: "admin", Port: adminPort, TargetPort: intstr.FromInt(adminPort)},
			},
		},
	}
}

// buildRPCService returns the headless Service used for stable per-pod DNS
// (RPC mesh, consumed by the meshing step in a later milestone).
func buildRPCService(cluster *v1alpha1.ObjectStore, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rpcSvcName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                "None",
			PublishNotReadyAddresses: true,
			Selector:                 commonLabels(cluster),
			Ports: []corev1.ServicePort{
				{Name: "rpc", Port: rpcPort, TargetPort: intstr.FromInt(rpcPort)},
			},
		},
	}
}

// garageContainer is the shared container spec for both profiles.
func garageContainer(image string) corev1.Container {
	return corev1.Container{
		Name:    "garage",
		Image:   image,
		Command: []string{"/garage", "server"},
		Env: []corev1.EnvVar{
			{Name: "GARAGE_CONFIG_FILE", Value: configMount + "/" + configFileName},
			{Name: "GARAGE_RPC_SECRET", ValueFrom: secretKeyRef(secretKeyRPC)},
			{Name: "GARAGE_ADMIN_TOKEN", ValueFrom: secretKeyRef(secretKeyAdmin)},
		},
		Ports: []corev1.ContainerPort{
			{Name: "s3", ContainerPort: s3Port},
			{Name: "rpc", ContainerPort: rpcPort},
			{Name: "admin", ContainerPort: adminPort},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: configMount},
			{Name: "data", MountPath: dataMountPath},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(s3Port)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
}

// secretKeyRef builds an EnvVarSource reading a key from the cluster secret.
// The secret name is patched in by the caller via the closure in podSpec.
func secretKeyRef(key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			Key: key,
			// LocalObjectReference.Name is filled in podSpec once the
			// cluster (hence secret name) is known.
		},
	}
}

// podSpec assembles the shared PodSpec (the data volume is provided by the
// caller: hostPath for System, PVC template for Lightweight).
func podSpec(cluster *v1alpha1.ObjectStore, image string, dataVolume *corev1.Volume) corev1.PodSpec {
	c := garageContainer(image)
	// Patch the secret name into the env sources now that we know the cluster.
	for i := range c.Env {
		if c.Env[i].ValueFrom != nil && c.Env[i].ValueFrom.SecretKeyRef != nil {
			c.Env[i].ValueFrom.SecretKeyRef.Name = secretName(cluster)
		}
	}

	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configName(cluster)},
				},
			},
		},
	}
	if dataVolume != nil {
		volumes = append(volumes, *dataVolume)
	}

	return corev1.PodSpec{
		ServiceAccountName: resourceName(cluster),
		ImagePullSecrets:   []corev1.LocalObjectReference{{Name: registryPullSecret}},
		// Garage must own its metadata/data directory. The base image runs as
		// a non-root user, but the backing volume is root-owned: a hostPath dir
		// is created root:root by the kubelet (System), and a fresh PVC mounts
		// root:root by default (Lightweight). fsGroup does not cover hostPath,
		// so the data-plane pod runs as root (consistent with how Deckhouse
		// storage data planes run); fsGroup keeps the PVC group-writable too.
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:  ptrInt64(0),
			RunAsGroup: ptrInt64(0),
			FSGroup:    ptrInt64(0),
		},
		Containers: []corev1.Container{c},
		Volumes:    volumes,
	}
}

func ptrInt64(v int64) *int64 { return &v }

// buildStatefulSet returns the StatefulSet for the Lightweight/Full profiles
// (PVC-backed). Full currently reuses the Garage layout until the SeaweedFS
// driver lands.
func buildStatefulSet(cluster *v1alpha1.ObjectStore, namespace, image, cfgHash string) *appsv1.StatefulSet {
	replicas := lightweightReplicas(cluster)
	spec := podSpec(cluster, image, nil) // data comes from volumeClaimTemplates

	if cluster.Spec.Placement != nil {
		spec.NodeSelector = cluster.Spec.Placement.NodeSelector
		spec.Tolerations = cluster.Spec.Placement.Tolerations
	}

	storageClass := ""
	if cluster.Spec.Storage != nil {
		storageClass = cluster.Spec.Storage.Class
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: rpcSvcName(cluster),
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: commonLabels(cluster)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      commonLabels(cluster),
					Annotations: map[string]string{annConfigHash: cfgHash},
				},
				Spec: spec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: commonLabels(cluster)},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: &storageClass,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize(cluster)},
						},
					},
				},
			},
		},
	}
}

// buildDaemonSet returns the DaemonSet for the System profile (hostPath on
// control-plane nodes).
func buildDaemonSet(cluster *v1alpha1.ObjectStore, namespace, image, cfgHash string) *appsv1.DaemonSet {
	hostPathType := corev1.HostPathDirectoryOrCreate
	dataVolume := &corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: fmt.Sprintf("%s/%s", hostPathBase, cluster.Name),
				Type: &hostPathType,
			},
		},
	}
	spec := podSpec(cluster, image, dataVolume)

	// System runs on control-plane nodes and must tolerate their taints.
	spec.NodeSelector = map[string]string{controlPlaneNodeLabel: ""}
	spec.Tolerations = []corev1.Toleration{
		{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: commonLabels(cluster)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      commonLabels(cluster),
					Annotations: map[string]string{annConfigHash: cfgHash},
				},
				Spec: spec,
			},
		},
	}
}
