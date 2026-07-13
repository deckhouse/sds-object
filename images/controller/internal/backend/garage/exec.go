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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// runningGaragePods returns the names of the Running Garage pods for the cluster,
// keyed by their StatefulSet ordinal (parsed from the pod name suffix). Used to
// capture each replica's node identity for persistence.
func (d *Driver) runningGaragePods(ctx context.Context, cluster *v1alpha1.ObjectStore) (map[int32]string, error) {
	pods := &corev1.PodList{}
	if err := d.apiReader.List(ctx, pods,
		client.InNamespace(d.namespace),
		client.MatchingLabels(commonLabels(cluster)),
	); err != nil {
		return nil, err
	}
	out := map[int32]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if ord, ok := podOrdinal(p.Name); ok {
			out[ord] = p.Name
		}
	}
	return out, nil
}

// execInPod runs cmd in a container of a pod, returning its stdout bytes. On
// failure the error carries stderr.
func (d *Driver) execInPod(ctx context.Context, pod, container string, cmd []string) ([]byte, error) {
	if d.restConfig == nil {
		return nil, fmt.Errorf("no rest config configured for exec")
	}
	cs, err := kubernetes.NewForConfig(d.restConfig)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return nil, fmt.Errorf("exec %v in %s: %w (stderr: %s)", cmd, pod, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
