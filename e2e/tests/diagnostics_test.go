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
	"strings"
	"time"

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

	// Every ObjectStore and Bucket, not just the shared ones: the profile specs
	// each bring up their own store, and a dump naming only the primary is silent
	// about exactly the object that failed — a Full store stuck below Ready cost a
	// whole diagnosis cycle to that gap, because its BackendReady message (the one
	// sentence that says why) was never printed.
	dumpAllDynamic(ctx, objectStoreGVR, "ObjectStore")
	dumpAllDynamic(ctx, bucketGVR, "Bucket")
	dumpDynamic(ctx, bucketClaimPolicyGVR, "", policyName(suiteCfg.bucketName), "BucketClaimPolicy")
	dumpDynamic(ctx, bucketAccessGVR, suiteCfg.namespace, accessName(suiteCfg.bucketName), "BucketAccess")

	dumpPods(ctx, moduleNS)
	dumpControllerLease(ctx)
	dumpControllerLog(ctx)
	dumpEvents(ctx, moduleNS)
	dumpEvents(ctx, suiteCfg.namespace)

	GinkgoWriter.Printf("================================================\n\n")
}

// dumpAllDynamic prints the status of every object of a cluster-scoped kind.
func dumpAllDynamic(ctx context.Context, gvr schema.GroupVersionResource, kind string) {
	list, err := suiteDyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("  %s list: %v\n", kind, err)
		return
	}
	if len(list.Items) == 0 {
		GinkgoWriter.Printf("  %s: none\n", kind)
		return
	}
	for i := range list.Items {
		dumpObject(&list.Items[i], kind, "")
	}
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

	dumpObject(obj, kind, ns)
}

// dumpObject prints one object's phase and conditions, messages included — the
// message is usually the whole answer.
func dumpObject(obj *unstructured.Unstructured, kind, ns string) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	GinkgoWriter.Printf("  %s %s: phase=%q\n", kind, formatRef(ns, obj.GetName()), phase)

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

// controllerLeaseName is the leader-election Lease the controller holds; its
// holder and renew time tell apart "the leader stopped working" from "leadership
// moved", which the log alone cannot.
const controllerLeaseName = "sds-object-controller"

// dumpControllerLease prints who holds the controller's leader lease and how
// stale the renewal is. A controller that stops reconciling looks the same in the
// log either way — wedged on a call, or no longer the leader — and this is the
// cheapest thing that separates the two.
func dumpControllerLease(ctx context.Context) {
	lease, err := suiteClientset.CoordinationV1().Leases(moduleNS).Get(ctx, controllerLeaseName, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("  controller lease: %v\n", err)
		return
	}
	holder, renew := "<none>", "<never>"
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	if lease.Spec.RenewTime != nil {
		renew = fmt.Sprintf("%s (%s ago)", lease.Spec.RenewTime.Format(time.RFC3339),
			time.Since(lease.Spec.RenewTime.Time).Truncate(time.Second))
	}
	GinkgoWriter.Printf("  controller lease: holder=%s renewed=%s\n", holder, renew)
}

// controllerLogTail is how much of the controller log a failure dump carries:
// enough to cover the reconciles around the failure without burying it.
const controllerLogTail = 120

// dumpControllerLog prints the tail of the module controller's log. Most failures
// are the controller not converging, and its log is the only place that says why —
// a stuck reconcile, a forbidden API call, a backend refusing a request. Without it
// a CI failure leaves only symptoms, and the cluster is gone by the time anyone
// looks.
func dumpControllerLog(ctx context.Context) {
	dep, err := suiteClientset.AppsV1().Deployments(moduleNS).Get(ctx, controllerDeploymentName, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("  controller Deployment: %v\n", err)
		return
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		GinkgoWriter.Printf("  controller selector: %v\n", err)
		return
	}
	pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		GinkgoWriter.Printf("  controller pods: %v\n", err)
		return
	}
	tail := int64(controllerLogTail)
	for i := range pods.Items {
		name := pods.Items[i].Name
		raw, err := suiteClientset.CoreV1().Pods(moduleNS).
			GetLogs(name, &corev1.PodLogOptions{Container: "controller", TailLines: &tail}).
			DoRaw(ctx)
		if err != nil {
			GinkgoWriter.Printf("  controller log (%s): %v\n", name, err)
			continue
		}
		GinkgoWriter.Printf("  controller log (%s, last %d lines):\n%s\n", name, controllerLogTail, strings.TrimRight(string(raw), "\n"))
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
