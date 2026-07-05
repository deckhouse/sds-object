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
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// SeaweedFS filer metadata is stored in a shared PostgreSQL provisioned via the
// managed-postgres module (managed-services.deckhouse.io/v1alpha1 Postgres),
// which lets the filer run with multiple replicas (HA). The filer uses the
// `postgres2` store, which auto-creates its tables (CREATE TABLE IF NOT EXISTS),
// so no manual schema bootstrap is needed.
const (
	pgDatabase = "seaweedfs"
	pgUser     = "seaweedfs"
	pgPort     = 5432
)

// postgresGVK is the managed-postgres Postgres CR.
var postgresGVK = schema.GroupVersionKind{
	Group: "managed-services.deckhouse.io", Version: "v1alpha1", Kind: "Postgres",
}

// pgName is the Postgres CR name for a cluster.
func pgName(cluster *v1alpha1.ObjectStorageCluster) string {
	return componentName(cluster, "pg")
}

// pgHost is the managed-postgres read-write Service the filer connects to.
// managed-postgres names the backing CNPG cluster "d8ms-pg-<pg>" and exposes a
// "<cnpg>-rw" Service, so this name is deterministic. The driver relies on it
// when the credentials Secret momentarily drops the "host" key — which
// managed-postgres does whenever the Postgres instance is briefly not serving
// (startup, failover), keeping only the stable username/password.
func pgHost(cluster *v1alpha1.ObjectStorageCluster) string {
	return "d8ms-pg-" + pgName(cluster) + "-rw"
}

// pgCredsSecretName is the Secret managed-postgres writes the filer user's
// credentials into (keys: host, username, password). The driver sets it via the
// user's storeCredsToSecret.
func pgCredsSecretName(cluster *v1alpha1.ObjectStorageCluster) string {
	return componentName(cluster, "pg") + "-creds"
}

// usesPostgres reports whether the filer metadata store is the shared
// managed-postgres database. Only HighRedundancy uses it (to run a multi-replica
// filer HA set); Single and Replicated use SeaweedFS's built-in leveldb store on
// a local PVC, which avoids the managed-postgres dependency but is single-filer.
func usesPostgres(cluster *v1alpha1.ObjectStorageCluster) bool {
	return cluster.Spec.Redundancy == v1alpha1.RedundancyHighRedundancy
}

// filerReplicas is the number of filer (S3 gateway) replicas, derived from the
// redundancy intent. Only HighRedundancy runs more than one, backed by the
// shared Postgres store; Single/Replicated run a single filer on a local
// leveldb store (a node-local store cannot be shared across replicas).
func filerReplicas(cluster *v1alpha1.ObjectStorageCluster) int32 {
	if usesPostgres(cluster) {
		return 3
	}
	return 1
}

// buildPostgres returns the managed-postgres Postgres CR backing the filer
// metadata store. The DB topology scales with the redundancy intent.
func buildPostgres(cluster *v1alpha1.ObjectStorageCluster, namespace string) *unstructured.Unstructured {
	instance := map[string]interface{}{
		"cpu":    map[string]interface{}{"cores": int64(1), "coreFraction": int64(100)},
		"memory": map[string]interface{}{"size": "512Mi"},
		"persistentVolumeClaim": map[string]interface{}{
			"size":             "2Gi",
			"storageClassName": storageClass(cluster),
		},
	}
	spec := map[string]interface{}{
		"postgresClassName": "default",
		"instance":          instance,
		"databases":         []interface{}{map[string]interface{}{"name": pgDatabase}},
		"users": []interface{}{map[string]interface{}{
			"name":               pgUser,
			"role":               "rw",
			"storeCredsToSecret": pgCredsSecretName(cluster),
		}},
	}

	// Single -> a standalone instance; otherwise an HA cluster.
	switch cluster.Spec.Redundancy {
	case v1alpha1.RedundancySingle:
		spec["type"] = "Standalone"
	case v1alpha1.RedundancyHighRedundancy:
		spec["type"] = "Cluster"
		spec["cluster"] = map[string]interface{}{"replication": "ConsistencyAndAvailability"}
	default: // Replicated
		spec["type"] = "Cluster"
		spec["cluster"] = map[string]interface{}{"replication": "Availability"}
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(postgresGVK)
	obj.SetName(pgName(cluster))
	obj.SetNamespace(namespace)
	obj.SetLabels(componentLabels(cluster, compFiler))
	obj.Object["spec"] = spec
	return obj
}

// renderFilerToml renders filer.toml configuring the postgres2 store. The
// managed-postgres rw endpoint enables TLS, so sslmode=require (encrypt without
// CA verification, matching managed-postgres' own DSN default).
func renderFilerToml(host string, port int, user, password, database string) string {
	// createTable is REQUIRED for the postgres2 store: SeaweedFS runs
	// fmt.Sprintf(createTable, <table>) for each metadata table (filemeta, one
	// per bucket). Without it the store formats an empty template and sends
	// `%!(EXTRA string=filemeta)` to Postgres — "syntax error at or near %!".
	// The "%%s" escape keeps a literal "%s" in the rendered TOML for SeaweedFS to
	// fill; the leading verbs below bind host/port/user/password/database in order.
	return fmt.Sprintf(`[postgres2]
enabled = true
hostname = "%s"
port = %d
username = "%s"
password = "%s"
database = "%s"
sslmode = "require"
createTable = """
CREATE TABLE IF NOT EXISTS "%%s" (
  dirhash   BIGINT,
  name      VARCHAR(65535),
  directory VARCHAR(65535),
  meta      bytea,
  PRIMARY KEY (dirhash, name)
);
"""
`, tomlBasicEscape(host), port, tomlBasicEscape(user), tomlBasicEscape(password), tomlBasicEscape(database))
}

// tomlBasicEscape escapes a value for embedding inside a TOML basic (double-
// quoted) string, so a password/host containing `"`, `\` or control characters
// cannot break the config or inject extra keys. Follows the TOML v1.0 basic
// string escape rules.
func tomlBasicEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(fmt.Sprintf(`\u%04X`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// renderFilerTomlLeveldb renders filer.toml configuring the built-in leveldb2
// store on the local data volume (used for Single/Replicated, no external DB).
func renderFilerTomlLeveldb(dir string) string {
	return fmt.Sprintf(`[leveldb2]
enabled = true
dir = "%s"
`, dir)
}

// buildFilerConfigSecret holds filer.toml (with the DB password), mounted by
// every filer replica. A Secret (not a ConfigMap) since it carries the password.
func buildFilerConfigSecret(cluster *v1alpha1.ObjectStorageCluster, namespace, filerToml string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      filerConfigName(cluster),
			Namespace: namespace,
			Labels:    componentLabels(cluster, compFiler),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"filer.toml": filerToml},
	}
}

func filerConfigName(cluster *v1alpha1.ObjectStorageCluster) string {
	return componentName(cluster, compFiler) + "-config"
}

// ensurePostgres creates or updates the managed-postgres Postgres CR backing
// the filer metadata store. Reads use the non-cached apiReader so a missing
// Postgres CRD (managed-postgres not installed) surfaces as a NoMatch error;
// writes go straight to the API server.
func (d *Driver) ensurePostgres(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) error {
	desired := buildPostgres(cluster, d.namespace)
	if err := controllerutil.SetControllerReference(cluster, desired, d.client.Scheme()); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(postgresGVK)
	err := d.apiReader.Get(ctx, client.ObjectKey{Namespace: d.namespace, Name: pgName(cluster)}, existing)
	if apierrors.IsNotFound(err) {
		return d.client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Overlay only the fields we manage and update only when something actually
	// changed. Replacing the whole spec unconditionally (the previous behaviour)
	// dropped any defaults managed-postgres writes back, so every reconcile bumped
	// the Postgres generation and kept managed-postgres perpetually re-syncing.
	before := existing.DeepCopy()

	// managed-postgres generates the user's password on first reconcile, writes
	// the plaintext to storeCredsToSecret and stores the resulting hash back into
	// spec.users[].hashedPassword (deleting the plaintext). Our desired users omit
	// it, so overlaying would strip hashedPassword — which makes managed-postgres
	// regenerate the password, desyncing the DB role from the credentials the
	// filer already read ("password authentication failed"). Carry the
	// managed-postgres-owned password fields over so the overlay preserves them.
	preservePgUserSecrets(desired, existing)

	spec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	if spec == nil {
		spec = map[string]interface{}{}
	}
	for k, v := range desired.Object["spec"].(map[string]interface{}) {
		spec[k] = v
	}
	existing.Object["spec"] = spec

	labels := existing.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range desired.GetLabels() {
		labels[k] = v
	}
	existing.SetLabels(labels)
	existing.SetOwnerReferences(desired.GetOwnerReferences())

	if reflect.DeepEqual(before.Object, existing.Object) {
		return nil
	}
	return d.client.Update(ctx, existing)
}

// preservePgUserSecrets copies the managed-postgres-owned password fields
// (hashedPassword / password) from the existing Postgres CR into the matching
// desired users (by name), so the overlay in ensurePostgres does not strip them
// and trigger a password regeneration.
func preservePgUserSecrets(desired, existing *unstructured.Unstructured) {
	desiredUsers, _, _ := unstructured.NestedSlice(desired.Object, "spec", "users")
	existingUsers, _, _ := unstructured.NestedSlice(existing.Object, "spec", "users")
	if len(desiredUsers) == 0 || len(existingUsers) == 0 {
		return
	}

	existingByName := make(map[string]map[string]interface{}, len(existingUsers))
	for _, u := range existingUsers {
		if m, ok := u.(map[string]interface{}); ok {
			if name, _ := m["name"].(string); name != "" {
				existingByName[name] = m
			}
		}
	}

	changed := false
	for _, u := range desiredUsers {
		m, ok := u.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		ex, ok := existingByName[name]
		if !ok {
			continue
		}
		for _, key := range []string{"hashedPassword", "password"} {
			if v, ok := ex[key]; ok {
				m[key] = v
				changed = true
			}
		}
	}
	if changed {
		_ = unstructured.SetNestedSlice(desired.Object, desiredUsers, "spec", "users")
	}
}

// pgCreds reads the filer DB credentials managed-postgres writes into the
// storeCredsToSecret Secret. Returns ok=false (not an error) while the Secret
// or its keys are not yet populated, so the caller can requeue.
func (d *Driver) pgCreds(ctx context.Context, cluster *v1alpha1.ObjectStorageCluster) (host, user, password string, ok bool, err error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: d.namespace, Name: pgCredsSecretName(cluster)}
	if err := d.apiReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			d.log.Info(fmt.Sprintf("[seaweedfs] pg creds Secret %s not found yet, waiting", key))
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	// Only username/password are required and stable. managed-postgres withdraws
	// "host" (and the dsn) from the Secret whenever the Postgres instance is not
	// serving, so depending on it would stall SeaweedFS for the whole startup;
	// instead fall back to the deterministic read-write Service and let the filer
	// retry the DB connection until Postgres is ready.
	user = string(secret.Data["username"])
	password = string(secret.Data["password"])
	if user == "" || password == "" {
		keys := make([]string, 0, len(secret.Data))
		for k := range secret.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d.log.Info(fmt.Sprintf("[seaweedfs] pg creds Secret %s present but incomplete: keys=[%s] userEmpty=%t passEmpty=%t",
			key, strings.Join(keys, ","), user == "", password == ""))
		return "", "", "", false, nil
	}
	host = string(secret.Data["host"])
	if host == "" {
		host = pgHost(cluster)
		d.log.Info(fmt.Sprintf("[seaweedfs] pg creds Secret %s has no host yet (Postgres not serving); using derived Service %q", key, host))
	}
	return host, user, password, true, nil
}

// isNoMatch reports a missing CRD (managed-postgres not installed).
func isNoMatch(err error) bool { return apimeta.IsNoMatchError(err) }
