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

package controller

import (
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

var testStages = []string{"A", "B"}

func cond(t string, s metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: reason}
}

func TestDerivePhase(t *testing.T) {
	cases := []struct {
		name  string
		conds []metav1.Condition
		want  string
	}{
		{"empty", nil, v1alpha1.PhasePending},
		{"error", []metav1.Condition{cond("A", metav1.ConditionFalse, reasonError)}, v1alpha1.PhaseError},
		{"inprogress", []metav1.Condition{cond("A", metav1.ConditionFalse, reasonInProgress)}, v1alpha1.PhaseInProgress},
		{"ready", []metav1.Condition{cond("A", metav1.ConditionTrue, reasonReady), cond("B", metav1.ConditionTrue, reasonReady)}, v1alpha1.PhaseReady},
		{"error wins over inprogress", []metav1.Condition{cond("A", metav1.ConditionFalse, reasonInProgress), cond("B", metav1.ConditionFalse, reasonError)}, v1alpha1.PhaseError},
	}
	for _, c := range cases {
		if got := derivePhase(c.conds, testStages); got != c.want {
			t.Errorf("%s: derivePhase=%q, want %q", c.name, got, c.want)
		}
	}
}

func TestAdvance(t *testing.T) {
	// Success advances and records True.
	sb := newStatusBuilder(1)
	if !advance(sb, testStages, "Ready", "A", true, "done", nil) {
		t.Fatalf("advance(done): want true")
	}
	if c := apimeta.FindStatusCondition(sb.conditions, "A"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("advance(done): A not True: %+v", c)
	}

	// Error stops and gates the aggregate.
	sb = newStatusBuilder(1)
	if advance(sb, testStages, "Ready", "A", false, "", errors.New("boom")) {
		t.Fatalf("advance(err): want false")
	}
	if c := apimeta.FindStatusCondition(sb.conditions, "A"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonError {
		t.Errorf("advance(err): A not False/Error: %+v", c)
	}
	if c := apimeta.FindStatusCondition(sb.conditions, "Ready"); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("advance(err): Ready not gated: %+v", c)
	}
}

func TestGateAfter(t *testing.T) {
	sb := newStatusBuilder(1)
	gateAfter(sb, []string{"A", "B", "C"}, "Ready", "A")

	for _, downstream := range []string{"B", "C", "Ready"} {
		c := apimeta.FindStatusCondition(sb.conditions, downstream)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForPrev {
			t.Errorf("gateAfter: %s not gated WaitingForPrev: %+v", downstream, c)
		}
	}
	// The gate-point stage itself is not touched by gateAfter.
	if c := apimeta.FindStatusCondition(sb.conditions, "A"); c != nil {
		t.Errorf("gateAfter must not write the afterStage condition, got %+v", c)
	}
}

func TestAggregateReady(t *testing.T) {
	sb := newStatusBuilder(1)
	if aggregateReady(sb, "Ready") {
		t.Errorf("aggregateReady(empty)=true, want false")
	}
	sb.setCondition("Ready", metav1.ConditionTrue, reasonReady, "")
	if !aggregateReady(sb, "Ready") {
		t.Errorf("aggregateReady(True)=false, want true")
	}
}
