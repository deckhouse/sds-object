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

package seaweedfs

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// SeaweedFS ports (all-in-one `weed server -s3`).
const (
	s3Port     = 8333
	filerPort  = 8888
	masterPort = 9333
	volumePort = 8080
)

const (
	dataMountPath      = "/data"
	registryPullSecret = "deckhouse-registry"
	// s3Region is the region advertised by the SeaweedFS S3 gateway.
	s3Region = "us-east-1"
)

func resourceName(cluster *v1alpha1.ObjectStorageCluster) string {
	return cluster.Name + "-seaweedfs"
}

func svcName(cluster *v1alpha1.ObjectStorageCluster) string { return resourceName(cluster) }

func commonLabels(cluster *v1alpha1.ObjectStorageCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":                "sds-object",
		"app.kubernetes.io/name":                      "seaweedfs",
		"storage.deckhouse.io/object-storage-cluster": cluster.Name,
	}
}

// s3Endpoint is the in-cluster S3 URL of the cluster's Service.
func s3Endpoint(cluster *v1alpha1.ObjectStorageCluster, namespace, clusterDomain string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", svcName(cluster), namespace, clusterDomain, s3Port)
}

// storageSize returns the PVC size, defaulting to 10Gi when unset/invalid.
func storageSize(cluster *v1alpha1.ObjectStorageCluster) resource.Quantity {
	if cluster.Spec.Storage != nil && cluster.Spec.Storage.Size != "" {
		if q, err := resource.ParseQuantity(cluster.Spec.Storage.Size); err == nil {
			return q
		}
	}
	return resource.MustParse("10Gi")
}

func storageClass(cluster *v1alpha1.ObjectStorageCluster) string {
	if cluster.Spec.Storage != nil {
		return cluster.Spec.Storage.Class
	}
	return ""
}

// buildService returns the ClusterIP Service exposing the S3 (and filer/master)
// APIs.
func buildService(cluster *v1alpha1.ObjectStorageCluster, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Spec: corev1.ServiceSpec{
			Selector: commonLabels(cluster),
			Ports: []corev1.ServicePort{
				{Name: "s3", Port: s3Port, TargetPort: intstr.FromInt(s3Port)},
				{Name: "filer", Port: filerPort, TargetPort: intstr.FromInt(filerPort)},
				{Name: "master", Port: masterPort, TargetPort: intstr.FromInt(masterPort)},
			},
		},
	}
}

// buildStatefulSet returns the all-in-one SeaweedFS StatefulSet.
//
// MVP: a single replica running `weed server -s3` (master + volume + filer +
// S3 gateway in one process), backed by one PVC. A distributed
// master/volume/filer topology and multi-replica redundancy are a follow-up.
func buildStatefulSet(cluster *v1alpha1.ObjectStorageCluster, namespace, image string) *appsv1.StatefulSet {
	replicas := int32(1)
	sc := storageClass(cluster)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cluster),
			Namespace: namespace,
			Labels:    commonLabels(cluster),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: svcName(cluster),
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: commonLabels(cluster)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: commonLabels(cluster)},
				Spec: corev1.PodSpec{
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
					// SeaweedFS must own its data dir on the root-owned PVC; the
					// base image runs as non-root and fsGroup is not enough on
					// all provisioners, so run as root (as Garage does).
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  ptrInt64(0),
						RunAsGroup: ptrInt64(0),
						FSGroup:    ptrInt64(0),
					},
					Containers: []corev1.Container{{
						Name:  "seaweedfs",
						Image: image,
						Command: []string{
							"/weed", "server",
							"-dir=" + dataMountPath,
							"-s3",
							"-ip=$(POD_IP)",
						},
						Env: []corev1.EnvVar{
							{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
							}},
						},
						Ports: []corev1.ContainerPort{
							{Name: "s3", ContainerPort: s3Port},
							{Name: "filer", ContainerPort: filerPort},
							{Name: "master", ContainerPort: masterPort},
							{Name: "volume", ContainerPort: volumePort},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: dataMountPath},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(s3Port)},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       10,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &sc,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize(cluster)},
					},
				},
			}},
		},
	}
}

func ptrInt64(v int64) *int64 { return &v }
