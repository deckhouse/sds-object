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
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// SeaweedFS ports.
const (
	s3Port      = 8333
	filerPort   = 8888
	filerGRPC   = 18888
	masterPort  = 9333
	masterGRPC  = 19333
	volumePort  = 8080
	volumeGRPC  = 18080
	dataMount   = "/data"
	configMount = "/etc/seaweedfs"
	// filerStoreDir is the leveldb2 metadata directory on the filer data PVC
	// (Single/Replicated profiles).
	filerStoreDir = dataMount + "/filerldb2"

	registryPullSecret = "deckhouse-registry"
	// s3Region is advertised by the SeaweedFS S3 gateway.
	s3Region = "us-east-1"
	// volumeSizeLimitMB caps a single SeaweedFS volume file so the volume
	// server's auto -max computation yields several volumes even on small PVCs.
	volumeSizeLimitMB = 1024
)

// Component names.
const (
	compMaster = "master"
	compVolume = "volume"
	compFiler  = "filer"
)

func resourceName(cluster *v1alpha1.ObjectStore) string {
	return cluster.Name + "-seaweedfs"
}

// svcName is the main S3 Service (selects the filer pods that run the S3
// gateway); the bucket/IAM code addresses the cluster through it.
func svcName(cluster *v1alpha1.ObjectStore) string { return resourceName(cluster) }

// componentName is the StatefulSet / headless Service name for a component.
func componentName(cluster *v1alpha1.ObjectStore, comp string) string {
	return resourceName(cluster) + "-" + comp
}

func commonLabels(cluster *v1alpha1.ObjectStore) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":      "sds-object",
		"app.kubernetes.io/name":            "seaweedfs",
		"storage.deckhouse.io/object-store": cluster.Name,
	}
}

// componentLabels are the pod/selector labels for one component.
func componentLabels(cluster *v1alpha1.ObjectStore, comp string) map[string]string {
	l := commonLabels(cluster)
	l["app.kubernetes.io/component"] = comp
	return l
}

// s3Endpoint is the in-cluster S3 URL of the cluster's main Service.
func s3Endpoint(cluster *v1alpha1.ObjectStore, namespace, clusterDomain string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", svcName(cluster), namespace, clusterDomain, s3Port)
}

// s3HostPort is the host:port of the S3 endpoint (no scheme), for the S3 client.
func s3HostPort(cluster *v1alpha1.ObjectStore, namespace, clusterDomain string) string {
	return fmt.Sprintf("%s.%s.svc.%s:%d", svcName(cluster), namespace, clusterDomain, s3Port)
}

// filerEndpoint is the in-cluster filer HTTP URL (for the S3 IAM config).
func filerEndpoint(cluster *v1alpha1.ObjectStore, namespace, clusterDomain string) string {
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", svcName(cluster), namespace, clusterDomain, filerPort)
}

// adminSecretName is the Secret (in the module namespace) holding the cluster's
// S3 admin credentials.
func adminSecretName(cluster *v1alpha1.ObjectStore) string {
	return resourceName(cluster) + "-admin"
}

// storageSize returns the per-volume-server PVC size, default 10Gi.
func storageSize(cluster *v1alpha1.ObjectStore) resource.Quantity {
	if cluster.Spec.Storage != nil && cluster.Spec.Storage.SizePerNode != "" {
		if q, err := resource.ParseQuantity(cluster.Spec.Storage.SizePerNode); err == nil {
			return q
		}
	}
	return resource.MustParse("10Gi")
}

func storageClass(cluster *v1alpha1.ObjectStore) string {
	if cluster.Spec.Storage != nil {
		return cluster.Spec.Storage.Class
	}
	return ""
}

// topology maps the redundancy intent to the master/volume replica counts and
// the SeaweedFS default replication code (xyz: other-DC/other-rack/other-server
// copies). Single is deployable on a one-node cluster.
func topology(cluster *v1alpha1.ObjectStore) (masters, volumes int32, replication string) {
	switch cluster.Spec.Redundancy {
	case v1alpha1.RedundancyNone:
		return 1, 1, "000"
	case v1alpha1.RedundancyHigh:
		return 3, 4, "002"
	default: // Replicated or unset
		return 3, 3, "001"
	}
}

// masterServers is the comma-separated list of master peer addresses used for
// both -peers (master raft) and -master (volume/filer). Resolved via the master
// headless Service per-pod DNS (the cluster search domain completes it).
func masterServers(cluster *v1alpha1.ObjectStore, namespace string) string {
	masters, _, _ := topology(cluster)
	name := componentName(cluster, compMaster)
	peers := make([]string, 0, masters)
	for i := int32(0); i < masters; i++ {
		peers = append(peers, fmt.Sprintf("%s-%d.%s.%s:%d", name, i, name, namespace, masterPort))
	}
	return strings.Join(peers, ",")
}

func ptrInt64(v int64) *int64 { return &v }

// podName is the downward-API env exposing the pod name (for -ip).
func podNameEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
	}}
}

func podIPEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
	}}
}

func rootSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{RunAsUser: ptrInt64(0), RunAsGroup: ptrInt64(0), FSGroup: ptrInt64(0)}
}

// filerStoreSize is the PVC size for the filer's local leveldb metadata store.
var filerStoreSize = resource.MustParse("2Gi")

// dataPVC is a per-pod PersistentVolumeClaim template named "data".
func dataPVC(cluster *v1alpha1.ObjectStore, size resource.Quantity) corev1.PersistentVolumeClaim {
	sc := storageClass(cluster)
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: commonLabels(cluster)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
}

// --- Services ---------------------------------------------------------------

// buildMainService is the ClusterIP Service exposing S3 (and filer) on the
// filer pods. This is the cluster's S3 endpoint.
func buildMainService(cluster *v1alpha1.ObjectStore, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName(cluster), Namespace: namespace, Labels: commonLabels(cluster)},
		Spec: corev1.ServiceSpec{
			Selector: componentLabels(cluster, compFiler),
			Ports: []corev1.ServicePort{
				{Name: "s3", Port: s3Port, TargetPort: intstr.FromInt(s3Port)},
				{Name: "filer", Port: filerPort, TargetPort: intstr.FromInt(filerPort)},
			},
		},
	}
}

// buildHeadlessService is the per-component headless Service for stable per-pod
// DNS (master raft peers, volume/filer registration).
func buildHeadlessService(cluster *v1alpha1.ObjectStore, namespace, comp string, ports []corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: componentName(cluster, comp), Namespace: namespace, Labels: componentLabels(cluster, comp)},
		Spec: corev1.ServiceSpec{
			ClusterIP:                "None",
			PublishNotReadyAddresses: true,
			Selector:                 componentLabels(cluster, comp),
			Ports:                    ports,
		},
	}
}

func buildMasterService(cluster *v1alpha1.ObjectStore, namespace string) *corev1.Service {
	return buildHeadlessService(cluster, namespace, compMaster, []corev1.ServicePort{
		{Name: "http", Port: masterPort, TargetPort: intstr.FromInt(masterPort)},
		{Name: "grpc", Port: masterGRPC, TargetPort: intstr.FromInt(masterGRPC)},
	})
}

func buildVolumeService(cluster *v1alpha1.ObjectStore, namespace string) *corev1.Service {
	return buildHeadlessService(cluster, namespace, compVolume, []corev1.ServicePort{
		{Name: "http", Port: volumePort, TargetPort: intstr.FromInt(volumePort)},
		{Name: "grpc", Port: volumeGRPC, TargetPort: intstr.FromInt(volumeGRPC)},
	})
}

func buildFilerService(cluster *v1alpha1.ObjectStore, namespace string) *corev1.Service {
	return buildHeadlessService(cluster, namespace, compFiler, []corev1.ServicePort{
		{Name: "http", Port: filerPort, TargetPort: intstr.FromInt(filerPort)},
		{Name: "grpc", Port: filerGRPC, TargetPort: intstr.FromInt(filerGRPC)},
		{Name: "s3", Port: s3Port, TargetPort: intstr.FromInt(s3Port)},
	})
}

// --- StatefulSets -----------------------------------------------------------

func tcpProbe(port int) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(port)}},
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
	}
}

func statefulSet(cluster *v1alpha1.ObjectStore, namespace, comp string, replicas int32, c corev1.Container, pvcs []corev1.PersistentVolumeClaim, volumes []corev1.Volume) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: componentName(cluster, comp), Namespace: namespace, Labels: componentLabels(cluster, comp)},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: componentName(cluster, comp),
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: componentLabels(cluster, comp)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: componentLabels(cluster, comp)},
				Spec: corev1.PodSpec{
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: registryPullSecret}},
					SecurityContext:  rootSecurityContext(),
					Containers:       []corev1.Container{c},
					Volumes:          volumes,
				},
			},
			VolumeClaimTemplates: pvcs,
		},
	}
}

func buildMasterStatefulSet(cluster *v1alpha1.ObjectStore, namespace, image string) *appsv1.StatefulSet {
	masters, _, replication := topology(cluster)
	name := componentName(cluster, compMaster)
	c := corev1.Container{
		Name:    "master",
		Image:   image,
		Command: []string{"/weed", "master"},
		Args: []string{
			"-port=" + strconv.Itoa(masterPort),
			"-mdir=" + dataMount,
			"-ip.bind=0.0.0.0",
			"-ip=$(POD_NAME)." + name + "." + namespace,
			"-peers=" + masterServers(cluster, namespace),
			"-defaultReplication=" + replication,
			"-volumeSizeLimitMB=" + strconv.Itoa(volumeSizeLimitMB),
		},
		Env:            []corev1.EnvVar{podNameEnv()},
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: masterPort}, {Name: "grpc", ContainerPort: masterGRPC}},
		VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: dataMount}},
		ReadinessProbe: tcpProbe(masterPort),
		Resources:      requests("100m", "256Mi"),
	}
	return statefulSet(cluster, namespace, compMaster, masters, c, []corev1.PersistentVolumeClaim{dataPVC(cluster, resource.MustParse("1Gi"))}, nil)
}

// volumeServers is the SeaweedFS volume-server replica count: spec.storage.nodes
// when set, otherwise the redundancy-derived topology value.
func volumeServers(cluster *v1alpha1.ObjectStore) int32 {
	_, volumes, _ := topology(cluster)
	if cluster.Spec.Storage != nil && cluster.Spec.Storage.Nodes != nil && *cluster.Spec.Storage.Nodes >= 1 {
		return *cluster.Spec.Storage.Nodes
	}
	return volumes
}

func buildVolumeStatefulSet(cluster *v1alpha1.ObjectStore, namespace, image string) *appsv1.StatefulSet {
	volumes := volumeServers(cluster)
	name := componentName(cluster, compVolume)
	c := corev1.Container{
		Name:    "volume",
		Image:   image,
		Command: []string{"/weed", "volume"},
		Args: []string{
			"-port=" + strconv.Itoa(volumePort),
			"-dir=" + dataMount,
			"-max=0",
			"-ip.bind=0.0.0.0",
			"-ip=$(POD_NAME)." + name + "." + namespace,
			"-mserver=" + masterServers(cluster, namespace),
		},
		Env:            []corev1.EnvVar{podNameEnv()},
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: volumePort}, {Name: "grpc", ContainerPort: volumeGRPC}},
		VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: dataMount}},
		ReadinessProbe: tcpProbe(volumePort),
		Resources:      requests("100m", "256Mi"),
	}
	return statefulSet(cluster, namespace, compVolume, volumes, c, []corev1.PersistentVolumeClaim{dataPVC(cluster, storageSize(cluster))}, nil)
}

// buildFilerStatefulSet runs the filer + S3 gateway. For HighRedundancy the
// filer metadata lives in the shared managed-postgres store (postgres2) so the
// replicas are stateless (no local PVC) and form an HA set. For Single/
// Replicated the filer uses the built-in leveldb2 store on a local `data` PVC,
// so it runs as a single replica. The store is selected entirely by the mounted
// filer.toml Secret. Started WITHOUT -s3.config so the gateway uses the
// filer-stored IAM config (/etc/iam/identity.json) the access reconciler
// manages and the gateway reloads automatically.
func buildFilerStatefulSet(cluster *v1alpha1.ObjectStore, namespace, image string) *appsv1.StatefulSet {
	mounts := []corev1.VolumeMount{{Name: "config", MountPath: configMount}}
	var pvcs []corev1.PersistentVolumeClaim
	if !usesPostgres(cluster) {
		mounts = append(mounts, corev1.VolumeMount{Name: "data", MountPath: dataMount})
		pvcs = []corev1.PersistentVolumeClaim{dataPVC(cluster, filerStoreSize)}
	}

	c := corev1.Container{
		Name:    "filer",
		Image:   image,
		Command: []string{"/weed", "filer"},
		Args: []string{
			"-port=" + strconv.Itoa(filerPort),
			"-ip.bind=0.0.0.0",
			"-ip=$(POD_IP)",
			"-master=" + masterServers(cluster, namespace),
			"-s3",
			"-s3.port=" + strconv.Itoa(s3Port),
		},
		Env: []corev1.EnvVar{podIPEnv()},
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: filerPort},
			{Name: "grpc", ContainerPort: filerGRPC},
			{Name: "s3", ContainerPort: s3Port},
		},
		VolumeMounts:   mounts,
		ReadinessProbe: tcpProbe(s3Port),
		Resources:      requests("100m", "256Mi"),
	}
	volumes := []corev1.Volume{{
		Name: "config",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: filerConfigName(cluster),
		}},
	}}
	return statefulSet(cluster, namespace, compFiler, filerReplicas(cluster), c, pvcs, volumes)
}

func requests(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}}
}
