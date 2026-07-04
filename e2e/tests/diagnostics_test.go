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

package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// dumpFailedSpecDiagnostics prints, on any spec failure, the state most useful
// for triage: the shared OSC, OSB, OSBPolicy and OSBAccess (status +
// conditions), the module's pods, and recent events in the module + test
// namespaces. All lookups are best-effort — diagnostics must never panic or mask
// the original failure.
func dumpFailedSpecDiagnostics(ctx context.Context) {
	GinkgoWriter.Printf("\n========== sds-object e2e diagnostics ==========\n")

	dumpDynamic(ctx, objectStorageClusterGVR, "", suiteCfg.oscName, "ObjectStorageCluster")
	dumpDynamic(ctx, objectStorageBucketGVR, "", suiteCfg.bucketName, "ObjectStorageBucket")
	dumpDynamic(ctx, objectStorageBucketPolicyGVR, "", policyName(suiteCfg.bucketName), "ObjectStorageBucketPolicy")
	dumpDynamic(ctx, objectStorageBucketAccessGVR, suiteCfg.namespace, accessName(suiteCfg.bucketName), "ObjectStorageBucketAccess")

	dumpPods(ctx, moduleNS)
	dumpEvents(ctx, moduleNS)
	dumpEvents(ctx, suiteCfg.namespace)

	GinkgoWriter.Printf("================================================\n\n")
}

func dumpDynamic(ctx context.Context, gvr schema.GroupVersionResource, ns, name, kind string) {
	var (
		obj *unstructured.Unstructured
		err error
	)
	if ns == "" {
		obj, err = suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		GinkgoWriter.Printf("  %s %s: %v\n", kind, formatRef(ns, name), err)
		return
	}

	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	GinkgoWriter.Printf("  %s %s: phase=%q\n", kind, formatRef(ns, name), phase)

	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(cm, "type")
		st, _, _ := unstructured.NestedString(cm, "status")
		reason, _, _ := unstructured.NestedString(cm, "reason")
		msg, _, _ := unstructured.NestedString(cm, "message")
		GinkgoWriter.Printf("    - %s=%s reason=%q msg=%q\n", t, st, reason, msg)
	}
}

func dumpPods(ctx context.Context, ns string) {
	pods, err := suiteClientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("  pods in %s: %v\n", ns, err)
		return
	}
	GinkgoWriter.Printf("  pods in %s (%d):\n", ns, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		GinkgoWriter.Printf("    - %s phase=%s ready=%v restarts=%d\n",
			p.Name, p.Status.Phase, podReady(p), podRestarts(p))
	}
}

func dumpEvents(ctx context.Context, ns string) {
	events, err := suiteClientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{Limit: 25})
	if err != nil {
		GinkgoWriter.Printf("  events in %s: %v\n", ns, err)
		return
	}
	GinkgoWriter.Printf("  recent events in %s:\n", ns)
	for i := range events.Items {
		e := &events.Items[i]
		GinkgoWriter.Printf("    - [%s] %s: %s\n", e.Type, e.Reason, trim(e.Message, 160))
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}

func trim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s…", s[:max])
}
