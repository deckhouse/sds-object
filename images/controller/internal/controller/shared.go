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

// Package controller holds the sds-object reconcilers. Each reconciler drives
// its CR through a small condition-based FSM: a fixed list of stage conditions
// is advanced in order, downstream stages are gated when an upstream stage is
// not Ready, and the aggregate Ready condition plus the coarse Phase are
// derived from the stage conditions. The backend-specific work behind each
// stage lives in the backend.Driver implementations.
package controller

import (
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// Finalizer is placed on every ObjectStore, Bucket and
// BucketAccess the controller reconciles, so backend teardown runs
// before the CR disappears.
const Finalizer = "storage.deckhouse.io/sds-object-controller"

// Common condition reasons shared across reconcilers.
const (
	reasonReady          = "Ready"
	reasonInProgress     = "InProgress"
	reasonError          = "Error"
	reasonWaitingForPrev = "WaitingForPrev"
)

// conditionReady is the aggregate readiness condition type. All reconciled CRs
// use it (ObjectStoreConditionReady, BucketConditionReady and BucketAccessConditionReady all equal
// "Ready"), so the shared FSM helpers gate it by this single invariant.
const conditionReady = "Ready"

// statusBuilder accumulates condition updates during a single reconcile and is
// flushed onto the CR status at the end. It is intentionally CR-agnostic: both
// reconcilers share it.
type statusBuilder struct {
	generation int64
	now        time.Time
	conditions []metav1.Condition
}

func newStatusBuilder(generation int64) *statusBuilder {
	return &statusBuilder{generation: generation, now: time.Now()}
}

func (s *statusBuilder) setCondition(condType string, condStatus metav1.ConditionStatus, reason, message string) {
	s.conditions = append(s.conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.generation,
		LastTransitionTime: metav1.NewTime(s.now),
	})
}

// advance records the outcome of a single stage on the status builder and
// reports whether the FSM may proceed to the next stage. When the stage is not
// done (error or in-progress), every downstream stage and the aggregate Ready
// are gated as False.
func advance(s *statusBuilder, stageOrder []string, condType string, done bool, message string, err error) bool {
	switch {
	case err != nil:
		s.setCondition(condType, metav1.ConditionFalse, reasonError, err.Error())
		gateAfter(s, stageOrder, condType)
	case !done:
		s.setCondition(condType, metav1.ConditionFalse, reasonInProgress, message)
		gateAfter(s, stageOrder, condType)
	default:
		s.setCondition(condType, metav1.ConditionTrue, reasonReady, message)
		return true
	}
	return false
}

// gateAfter marks every stage strictly after afterStage, plus the aggregate
// Ready condition, as False/WaitingForPrev.
func gateAfter(s *statusBuilder, stageOrder []string, afterStage string) {
	startIdx := -1
	for i, t := range stageOrder {
		if t == afterStage {
			startIdx = i + 1
			break
		}
	}
	if startIdx >= 0 {
		for _, t := range stageOrder[startIdx:] {
			s.setCondition(t, metav1.ConditionFalse, reasonWaitingForPrev,
				fmt.Sprintf("waiting for %s", afterStage))
		}
	}
	s.setCondition(conditionReady, metav1.ConditionFalse, reasonWaitingForPrev,
		fmt.Sprintf("waiting for %s", afterStage))
}

// derivePhase converts the FSM stage conditions into the coarse Phase. Only the
// stage conditions in stageOrder are consulted (the aggregate Ready is ignored
// here), matching the sds-elastic semantics.
func derivePhase(conditions []metav1.Condition, stageOrder []string) string {
	if len(conditions) == 0 {
		return v1alpha1.PhasePending
	}
	hasError := false
	hasFalse := false
	for _, t := range stageOrder {
		c := apimeta.FindStatusCondition(conditions, t)
		if c == nil || c.Status != metav1.ConditionFalse {
			continue
		}
		hasFalse = true
		if c.Reason == reasonError {
			hasError = true
		}
	}
	if hasError {
		return v1alpha1.PhaseError
	}
	if hasFalse {
		return v1alpha1.PhaseInProgress
	}
	return v1alpha1.PhaseReady
}

// aggregateReady reports whether the builder's most recent write of the
// aggregate Ready condition is True.
func aggregateReady(s *statusBuilder) bool {
	for i := len(s.conditions) - 1; i >= 0; i-- {
		if s.conditions[i].Type == conditionReady {
			return s.conditions[i].Status == metav1.ConditionTrue
		}
	}
	return false
}
