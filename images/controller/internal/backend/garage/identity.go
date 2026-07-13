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
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// Garage node identity (ed25519) file layout inside the metadata dir and the
// expected raw key sizes, used to validate a captured identity before persisting
// it.
const (
	nodeKeyPath      = dataMountPath + "/meta/node_key"
	nodeKeyPubPath   = nodeKeyPath + ".pub"
	nodeKeyPrivBytes = 64
	nodeKeyPubBytes  = 32
)

// nodeIdentityDataKey is the identity-Secret key holding the private node_key for
// the given replica ordinal; the public key is that plus ".pub". These match the
// filenames the restore-node-key initContainer reads from the mounted Secret.
func nodeIdentityDataKey(ord int32) string {
	return fmt.Sprintf("node-%d", ord)
}

// podOrdinal parses the StatefulSet ordinal from a pod name (the integer after
// the last "-"), returning false when the suffix is not a number.
func podOrdinal(name string) (int32, bool) {
	i := strings.LastIndex(name, "-")
	if i < 0 || i == len(name)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// ensureNodeIdentities persists each System replica's Garage node identity so a
// recycled replica keeps a stable node ID (see restoreNodeKeyScript). It ensures
// the identity Secret exists, then captures the node_key/node_key.pub of every
// Running replica whose identity is not yet stored. Keys are stable, so an
// already-captured ordinal is never overwritten. Capture is best-effort: a pod
// not answering yet is retried on the next reconcile; only genuine API errors are
// returned.
func (d *Driver) ensureNodeIdentities(ctx context.Context, cluster *v1alpha1.ObjectStore) error {
	if d.restConfig == nil {
		return nil // no exec transport (e.g. in tests) — nothing to capture
	}

	secret, err := d.ensureNodeIdentitySecret(ctx, cluster)
	if err != nil {
		return err
	}

	pods, err := d.runningGaragePods(ctx, cluster)
	if err != nil {
		return err
	}

	captured := map[string][]byte{}
	for ord, pod := range pods {
		privKey := nodeIdentityDataKey(ord)
		pubKey := privKey + ".pub"
		if len(secret.Data[privKey]) == nodeKeyPrivBytes && len(secret.Data[pubKey]) == nodeKeyPubBytes {
			continue // already captured; the identity is stable, do not overwrite
		}
		priv, err := d.captureNodeKey(ctx, pod, nodeKeyPath)
		if err != nil {
			d.log.Warning(fmt.Sprintf("[ensureNodeIdentities] read node_key from %s: %v", pod, err))
			continue
		}
		pub, err := d.captureNodeKey(ctx, pod, nodeKeyPubPath)
		if err != nil {
			d.log.Warning(fmt.Sprintf("[ensureNodeIdentities] read node_key.pub from %s: %v", pod, err))
			continue
		}
		if len(priv) != nodeKeyPrivBytes || len(pub) != nodeKeyPubBytes {
			d.log.Warning(fmt.Sprintf("[ensureNodeIdentities] %s: unexpected identity sizes (priv=%d want %d, pub=%d want %d)", pod, len(priv), nodeKeyPrivBytes, len(pub), nodeKeyPubBytes))
			continue
		}
		captured[privKey] = priv
		captured[pubKey] = pub
	}

	if len(captured) == 0 {
		return nil
	}
	return d.storeNodeIdentities(ctx, cluster, captured)
}

// ensureNodeIdentitySecret returns the identity Secret, creating an empty
// owned one if it does not exist yet.
func (d *Driver) ensureNodeIdentitySecret(ctx context.Context, cluster *v1alpha1.ObjectStore) (*corev1.Secret, error) {
	key := client.ObjectKey{Namespace: d.namespace, Name: nodeIdentitySecretName(cluster)}
	secret := &corev1.Secret{}
	err := d.client.Get(ctx, key, secret)
	if err == nil {
		return secret, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeIdentitySecretName(cluster),
			Namespace: d.namespace,
			Labels:    commonLabels(cluster),
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := controllerutil.SetControllerReference(cluster, secret, d.client.Scheme()); err != nil {
		return nil, err
	}
	if err := d.client.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	if err := d.client.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// storeNodeIdentities merges the captured node identities into the identity
// Secret, never overwriting a key that is already present (identities are
// stable). Retries on write conflict.
func (d *Driver) storeNodeIdentities(ctx context.Context, cluster *v1alpha1.ObjectStore, captured map[string][]byte) error {
	key := client.ObjectKey{Namespace: d.namespace, Name: nodeIdentitySecretName(cluster)}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		secret := &corev1.Secret{}
		if err := d.client.Get(ctx, key, secret); err != nil {
			return err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		changed := false
		for k, v := range captured {
			if len(secret.Data[k]) != 0 {
				continue // keep the already-persisted identity
			}
			secret.Data[k] = v
			changed = true
		}
		if !changed {
			return nil
		}
		return d.client.Update(ctx, secret)
	})
}

// captureNodeKey reads a Garage identity file from a Running pod and returns its
// raw bytes. The file is transported base64-encoded (binary-safe over the exec
// stream) and any wrapping whitespace is stripped before decoding.
func (d *Driver) captureNodeKey(ctx context.Context, pod, path string) ([]byte, error) {
	out, err := d.execInPod(ctx, pod, "garage", []string{"base64", path})
	if err != nil {
		return nil, err
	}
	enc := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, string(out))
	return base64.StdEncoding.DecodeString(enc)
}
