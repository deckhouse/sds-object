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
	"fmt"

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
)

// resourceName is the common name/prefix for every object backing a cluster.
func resourceName(cluster *v1alpha1.ObjectStorageCluster) string {
	return cluster.Name + "-garage"
}

func secretName(cluster *v1alpha1.ObjectStorageCluster) string {
	return resourceName(cluster) + "-secrets"
}
func configName(cluster *v1alpha1.ObjectStorageCluster) string {
	return resourceName(cluster) + "-config"
}
func rpcSvcName(cluster *v1alpha1.ObjectStorageCluster) string { return resourceName(cluster) + "-rpc" }
func s3SvcName(cluster *v1alpha1.ObjectStorageCluster) string  { return resourceName(cluster) }

// commonLabels are placed on every object owned by a cluster.
func commonLabels(cluster *v1alpha1.ObjectStorageCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":                "sds-object",
		"app.kubernetes.io/name":                      "garage",
		"storage.deckhouse.io/object-storage-cluster": cluster.Name,
	}
}

// s3Endpoint is the in-cluster S3 URL of the cluster's Service.
func s3Endpoint(cluster *v1alpha1.ObjectStorageCluster, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", s3SvcName(cluster), namespace, s3Port)
}

// replicationFactor maps the high-level redundancy intent to a Garage
// replication_factor. The empty value defaults to Replicated.
func replicationFactor(cluster *v1alpha1.ObjectStorageCluster) int32 {
	switch cluster.Spec.Redundancy {
	case v1alpha1.RedundancySingle:
		return 1
	case v1alpha1.RedundancyHighRedundancy:
		return 5
	default: // RedundancyReplicated or unset
		return 3
	}
}

// lightweightReplicas is the StatefulSet replica count for the Lightweight
// profile, derived from the redundancy intent.
func lightweightReplicas(cluster *v1alpha1.ObjectStorageCluster) int32 {
	return replicationFactor(cluster)
}

// storageSize returns the per-node PVC size for PVC-backed profiles, defaulting
// to 10Gi when spec.storage.size is unset/invalid.
func storageSize(cluster *v1alpha1.ObjectStorageCluster) resource.Quantity {
	if cluster.Spec.Storage != nil && cluster.Spec.Storage.Size != "" {
		if q, err := resource.ParseQuantity(cluster.Spec.Storage.Size); err == nil {
			return q
		}
	}
	return resource.MustParse("10Gi")
}

// renderConfig produces the garage.toml content. Secrets (rpc, admin token) are
// supplied via environment variables, not the file.
func renderConfig(cluster *v1alpha1.ObjectStorageCluster) string {
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
`, dataMountPath, dataMountPath, replicationFactor(cluster), rpcPort, s3Port, adminPort)
}

// buildConfigMap returns the ConfigMap holding garage.toml.
func buildConfigMap(cluster *v1alpha1.ObjectStorageCluster, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Data: map[string]string{configFileName: renderConfig(cluster)},
	}
}

// buildServiceAccount returns the ServiceAccount the Garage pods run as.
func buildServiceAccount(cluster *v1alpha1.ObjectStorageCluster, namespace string) *corev1.ServiceAccount {
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
func buildS3Service(cluster *v1alpha1.ObjectStorageCluster, namespace string) *corev1.Service {
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
func buildRPCService(cluster *v1alpha1.ObjectStorageCluster, namespace string) *corev1.Service {
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
func podSpec(cluster *v1alpha1.ObjectStorageCluster, image string, dataVolume *corev1.Volume) corev1.PodSpec {
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
		Containers:         []corev1.Container{c},
		Volumes:            volumes,
	}
}

// buildStatefulSet returns the StatefulSet for the Lightweight/Full profiles
// (PVC-backed). Full currently reuses the Garage layout until the SeaweedFS
// driver lands.
func buildStatefulSet(cluster *v1alpha1.ObjectStorageCluster, namespace, image string) *appsv1.StatefulSet {
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
				ObjectMeta: metav1.ObjectMeta{Labels: commonLabels(cluster)},
				Spec:       spec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
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
func buildDaemonSet(cluster *v1alpha1.ObjectStorageCluster, namespace, image string) *appsv1.DaemonSet {
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
	spec.NodeSelector = map[string]string{"node-role.kubernetes.io/control-plane": ""}
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
				ObjectMeta: metav1.ObjectMeta{Labels: commonLabels(cluster)},
				Spec:       spec,
			},
		},
	}
}
