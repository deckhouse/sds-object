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

// Package seaweedfs implements the backend.Driver for the SeaweedFS object
// storage backend (the Full cluster profile).
//
// Data plane: a distributed topology of master, volume and filer (+S3 gateway)
// StatefulSets. The filer runs with multiple replicas (HA) backed by a shared
// PostgreSQL metadata store provisioned via the managed-postgres module (see
// postgres.go).
//
// Bucket/credential provisioning uses SeaweedFS's filer-stored S3 IAM config
// (/etc/iam/identity.json, managed over the filer HTTP API; the S3 gateway
// subscribes to filer metadata and reloads it) plus the S3 API for the bucket
// itself. EnsureCluster bootstraps an admin identity used for bucket
// create/delete; each BucketAccess gets its own identity scoped to
// its bucket.
package seaweedfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/internal/backend/s3util"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

// seaweedfsVersion is the SeaweedFS release this driver targets. Keep it in
// sync with the upstream tag pinned in images/seaweedfs/werf.inc.yaml.
const seaweedfsVersion = "3.71"

// adminIdentityName is the IAM identity used by the controller to manage
// buckets via the S3 API.
const adminIdentityName = "sds-object-admin"

// Keys in the admin secret.
const (
	secretKeyAccessKey = "accessKey"
	secretKeySecretKey = "secretKey"
)

// Driver reconciles SeaweedFS clusters.
type Driver struct {
	client        client.Client
	apiReader     client.Reader
	log           *logger.Logger
	namespace     string
	image         string
	clusterDomain string
}

var _ backend.Driver = (*Driver)(nil)

// New builds a SeaweedFS Driver.
func New(c client.Client, apiReader client.Reader, log *logger.Logger, namespace, image, clusterDomain string) *Driver {
	return &Driver{client: c, apiReader: apiReader, log: log, namespace: namespace, image: image, clusterDomain: clusterDomain}
}

func (d *Driver) Type() v1alpha1.BackendType { return v1alpha1.BackendSeaweedFS }

// EnsureCluster reconciles the SeaweedFS data plane and bootstraps the S3 admin
// identity, then reports the cluster state.
func (d *Driver) EnsureCluster(ctx context.Context, cluster *v1alpha1.ObjectStore) (backend.ClusterState, error) {
	state := backend.ClusterState{
		Backend: v1alpha1.BackendStatus{Type: v1alpha1.BackendSeaweedFS, Version: seaweedfsVersion},
	}

	if d.image == "" {
		state.Message = "SEAWEEDFS_IMAGE is not configured on the controller"
		return state, nil
	}

	// Services first, so the workloads can resolve peers.
	for _, obj := range []client.Object{
		buildMainService(cluster, d.namespace),
		buildMasterService(cluster, d.namespace),
		buildVolumeService(cluster, d.namespace),
		buildFilerService(cluster, d.namespace),
	} {
		if err := d.apply(ctx, cluster, obj); err != nil {
			return state, fmt.Errorf("ensure %T: %w", obj, err)
		}
	}

	// Filer metadata store. HighRedundancy uses a shared managed-postgres DB
	// (enables a multi-replica filer HA set); Single/Replicated use the
	// built-in leveldb2 store on a local PVC (no external dependency).
	var filerToml string
	if usesPostgres(cluster) {
		if err := d.ensurePostgres(ctx, cluster); err != nil {
			if isNoMatch(err) {
				state.Message = "Postgres CRD not found; is the managed-postgres module installed?"
				return state, nil
			}
			return state, fmt.Errorf("ensure Postgres: %w", err)
		}
		host, user, pass, ok, err := d.pgCreds(ctx, cluster)
		if err != nil {
			return state, fmt.Errorf("read Postgres credentials: %w", err)
		}
		if !ok {
			state.Message = "waiting for managed-postgres to provision filer database credentials"
			return state, nil
		}
		filerToml = renderFilerToml(host, pgPort, user, pass, pgDatabase)
	} else {
		filerToml = renderFilerTomlLeveldb(filerStoreDir)
	}
	cfgSecret := buildFilerConfigSecret(cluster, d.namespace, filerToml)
	if err := d.apply(ctx, cluster, cfgSecret); err != nil {
		return state, fmt.Errorf("ensure filer config: %w", err)
	}

	// master -> volume -> filer.
	workloads := []struct {
		comp string
		sts  *appsv1.StatefulSet
	}{
		{compMaster, buildMasterStatefulSet(cluster, d.namespace, d.image)},
		{compVolume, buildVolumeStatefulSet(cluster, d.namespace, d.image)},
		{compFiler, buildFilerStatefulSet(cluster, d.namespace, d.image)},
	}
	for _, w := range workloads {
		if err := d.apply(ctx, cluster, w.sts); err != nil {
			return state, fmt.Errorf("ensure %s statefulset: %w", w.comp, err)
		}
	}
	for _, w := range workloads {
		desired := int32(0)
		if w.sts.Spec.Replicas != nil {
			desired = *w.sts.Spec.Replicas
		}
		if w.sts.Status.ReadyReplicas < desired {
			state.Message = fmt.Sprintf("SeaweedFS %s rolling out (%d/%d pods ready)", w.comp, w.sts.Status.ReadyReplicas, desired)
			return state, nil
		}
	}

	// Bootstrap the S3 admin identity in the filer-stored IAM config. This also
	// switches the S3 gateway from anonymous to authenticated access.
	if err := d.ensureAdminIdentity(ctx, cluster); err != nil {
		state.Message = fmt.Sprintf("configuring S3 admin identity: %v", err)
		return state, nil
	}

	state.Ready = true
	state.Message = "SeaweedFS S3 gateway is ready"
	state.Endpoint = v1alpha1.EndpointStatus{Internal: s3Endpoint(cluster, d.namespace, d.clusterDomain), Region: s3Region}
	return state, nil
}

// DeleteCluster relies on owner-reference GC for the workloads and Services.
// The StatefulSet PVCs (volume servers, and the filer's leveldb store) are not
// garbage-collected by Kubernetes, so they persist by default (Retain). Only
// when the cluster reclaim policy is Delete are they removed.
func (d *Driver) DeleteCluster(ctx context.Context, cluster *v1alpha1.ObjectStore) error {
	if cluster.Spec.ReclaimPolicy != v1alpha1.ClusterReclaimDelete {
		return nil
	}
	return backend.DeleteClusterPVCs(ctx, d.client, d.namespace, commonLabels(cluster))
}

// EnsureBucket creates the bucket via the S3 API with the admin credentials
// (no per-bucket key — access keys are issued per BucketAccess).
// Idempotent.
func (d *Driver) EnsureBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) (backend.BucketState, error) {
	adminAK, adminSK, err := d.adminCreds(ctx, cluster)
	if err != nil {
		return backend.BucketState{}, err
	}
	if adminAK == "" {
		return backend.BucketState{Message: "S3 admin identity is not provisioned yet"}, nil
	}

	name := backend.BucketDisplayName(bucket)
	mc, err := s3util.NewClient(s3HostPort(cluster, d.namespace, d.clusterDomain), adminAK, adminSK)
	if err != nil {
		return backend.BucketState{}, fmt.Errorf("build S3 client: %w", err)
	}
	if err := s3util.EnsureBucket(ctx, mc, name, s3Region); err != nil {
		return backend.BucketState{}, err
	}

	return backend.BucketState{Ready: true, Message: "bucket provisioned", BucketName: name}, nil
}

// DeleteBucket removes the bucket when the reclaim policy is Delete. Access
// keys (IAM identities) are removed separately by DeleteAccess. Idempotent.
func (d *Driver) DeleteBucket(ctx context.Context, cluster *v1alpha1.ObjectStore, bucket *v1alpha1.Bucket) error {
	if bucket.Spec.ReclaimPolicy != v1alpha1.BucketReclaimDelete {
		return nil
	}

	adminAK, adminSK, err := d.adminCreds(ctx, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // cluster admin secret gone: nothing to clean up
		}
		return err
	}
	if adminAK == "" {
		return nil
	}

	mc, err := s3util.NewClient(s3HostPort(cluster, d.namespace, d.clusterDomain), adminAK, adminSK)
	if err != nil {
		return fmt.Errorf("build S3 client: %w", err)
	}
	return s3util.DeleteBucket(ctx, mc, backend.BucketDisplayName(bucket))
}

// ensureAdminIdentity makes sure the admin Secret exists and the matching
// admin identity is present in the filer IAM config.
func (d *Driver) ensureAdminIdentity(ctx context.Context, cluster *v1alpha1.ObjectStore) error {
	ak, sk, err := d.ensureAdminSecret(ctx, cluster)
	if err != nil {
		return err
	}

	filer := newFilerClient(filerEndpoint(cluster, d.namespace, d.clusterDomain))
	cfg, err := filer.readIdentities(ctx)
	if err != nil {
		return err
	}
	admin := s3Identity{
		Name:        adminIdentityName,
		Credentials: []s3Credential{{AccessKey: ak, SecretKey: sk}},
		Actions:     []string{actionAdmin},
	}
	if cfg.upsert(admin) {
		if err := filer.writeIdentities(ctx, cfg); err != nil {
			return err
		}
	}
	return nil
}

// ensureAdminSecret creates the cluster admin Secret on first reconcile and
// returns its credentials. It never overwrites existing values.
func (d *Driver) ensureAdminSecret(ctx context.Context, cluster *v1alpha1.ObjectStore) (string, string, error) {
	key := client.ObjectKey{Namespace: d.namespace, Name: adminSecretName(cluster)}
	existing := &corev1.Secret{}
	err := d.client.Get(ctx, key, existing)
	if err == nil {
		return string(existing.Data[secretKeyAccessKey]), string(existing.Data[secretKeySecretKey]), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", "", err
	}

	ak, err := randomHex(16)
	if err != nil {
		return "", "", err
	}
	sk, err := randomHex(32)
	if err != nil {
		return "", "", err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adminSecretName(cluster),
			Namespace: d.namespace,
			Labels:    commonLabels(cluster),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyAccessKey: []byte(ak),
			secretKeySecretKey: []byte(sk),
		},
	}
	if err := controllerutil.SetControllerReference(cluster, secret, d.client.Scheme()); err != nil {
		return "", "", err
	}
	if err := d.client.Create(ctx, secret); err != nil {
		return "", "", err
	}
	return ak, sk, nil
}

// adminCreds reads the cluster admin credentials (empty when not bootstrapped).
func (d *Driver) adminCreds(ctx context.Context, cluster *v1alpha1.ObjectStore) (string, string, error) {
	secret := &corev1.Secret{}
	if err := d.apiReader.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: adminSecretName(cluster)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", err
	}
	return string(secret.Data[secretKeyAccessKey]), string(secret.Data[secretKeySecretKey]), nil
}

// apply creates or updates obj, setting the cluster as its controller owner.
func (d *Driver) apply(ctx context.Context, cluster *v1alpha1.ObjectStore, obj client.Object) error {
	desired := obj.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, d.client, obj, func() error {
		mergeDesired(obj, desired)
		return controllerutil.SetControllerReference(cluster, obj, d.client.Scheme())
	})
	return err
}

// mergeDesired copies the desired spec onto the live object fetched by
// CreateOrUpdate, preserving server-managed metadata.
func mergeDesired(live, desired client.Object) {
	switch l := live.(type) {
	case *corev1.Service:
		d := desired.(*corev1.Service)
		l.Labels = d.Labels
		l.Spec.Selector = d.Spec.Selector
		l.Spec.Ports = d.Spec.Ports
	case *corev1.Secret:
		d := desired.(*corev1.Secret)
		l.Labels = d.Labels
		l.Type = d.Type
		l.StringData = d.StringData
	case *appsv1.StatefulSet:
		d := desired.(*appsv1.StatefulSet)
		l.Labels = d.Labels
		l.Spec.Replicas = d.Spec.Replicas
		l.Spec.Template = d.Spec.Template
	}
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
