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
	"bytes"
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// skipDeadNodes forces Garage to stop waiting for removed-but-dead nodes to
// drain their data, so a layout change that dropped such a node can finalize and
// the cluster returns to healthy. Removing a node normally triggers a graceful
// drain, but a dead node (a replica whose master was removed or which was
// recycled) can never confirm it, leaving the cluster "degraded" indefinitely.
// The Garage v1 admin API exposes no equivalent, so this runs the
// `garage layout skip-dead-nodes` CLI inside a Running Garage pod. version is the
// layout version to assume current (the one just applied). Best-effort: the
// caller logs failures without failing the reconcile.
func (d *Driver) skipDeadNodes(ctx context.Context, cluster *v1alpha1.ObjectStore, version int) error {
	if d.restConfig == nil {
		return fmt.Errorf("no rest config configured for exec")
	}
	pod, err := d.runningGaragePod(ctx, cluster)
	if err != nil {
		return err
	}
	cmd := []string{"/garage", "layout", "skip-dead-nodes", "--version", strconv.Itoa(version), "--allow-missing-data"}
	return d.execInPod(ctx, pod, "garage", cmd)
}

// runningGaragePod returns the name of one Running Garage pod for the cluster.
func (d *Driver) runningGaragePod(ctx context.Context, cluster *v1alpha1.ObjectStore) (string, error) {
	pods := &corev1.PodList{}
	if err := d.apiReader.List(ctx, pods,
		client.InNamespace(d.namespace),
		client.MatchingLabels(commonLabels(cluster)),
	); err != nil {
		return "", err
	}
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return pods.Items[i].Name, nil
		}
	}
	return "", fmt.Errorf("no Running Garage pod to exec into")
}

// execInPod runs cmd in a container of a pod, returning an error that carries
// stderr on failure.
func (d *Driver) execInPod(ctx context.Context, pod, container string, cmd []string) error {
	cs, err := kubernetes.NewForConfig(d.restConfig)
	if err != nil {
		return err
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(d.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(d.restConfig, "POST", req.URL())
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return fmt.Errorf("exec %v in %s: %w (stderr: %s)", cmd, pod, err, stderr.String())
	}
	return nil
}
