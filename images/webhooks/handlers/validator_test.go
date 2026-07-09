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

package handlers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func listKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		objectStoreGVR:       "ObjectStoreList",
		bucketGVR:            "BucketList",
		bucketClaimPolicyGVR: "BucketClaimPolicyList",
		elasticClusterGVR:    "ElasticClusterList",
	}
}

func obObj(name, bucketName string) *unstructured.Unstructured {
	spec := map[string]interface{}{"objectStoreRef": "c1"}
	if bucketName != "" {
		spec["bucketName"] = bucketName
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "Bucket",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       spec,
	}}
}

func oscObj(name, clusterType string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.deckhouse.io/v1alpha1",
		"kind":       "ObjectStore",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"type": clusterType},
	}}
}

func newFakeValidator(objs ...runtime.Object) *Validator {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds(), objs...)
	return NewValidator(dyn)
}

func TestEffectiveBucketName(t *testing.T) {
	if got := effectiveBucketName(obObj("data", "")); got != "data" {
		t.Errorf("effectiveBucketName(default)=%q, want data", got)
	}
	if got := effectiveBucketName(obObj("data", "custom")); got != "custom" {
		t.Errorf("effectiveBucketName(explicit)=%q, want custom", got)
	}
}

func TestBucketValidate(t *testing.T) {
	// Existing bucket "x" on cluster c1.
	v := newFakeValidator(obObj("x", ""))

	// Duplicate effective name on the same cluster -> deny.
	dup := obObj("y", "x")
	res, err := v.BucketValidate(context.Background(), nil, dup)
	if err != nil {
		t.Fatalf("BucketValidate(dup): %v", err)
	}
	if res.Valid {
		t.Errorf("BucketValidate(dup): want deny, got allow")
	}

	// Unique name -> allow (clusterRef missing only yields a warning).
	uniq := obObj("y", "")
	res, err = v.BucketValidate(context.Background(), nil, uniq)
	if err != nil {
		t.Fatalf("BucketValidate(uniq): %v", err)
	}
	if !res.Valid {
		t.Errorf("BucketValidate(uniq): want allow, got deny (%s)", res.Message)
	}
}

func TestObjectStoreValidate(t *testing.T) {
	v := newFakeValidator(oscObj("system1", "System"))

	// A second System cluster -> deny.
	res, err := v.ObjectStoreValidate(context.Background(), nil, oscObj("system2", "System"))
	if err != nil {
		t.Fatalf("ObjectStoreValidate(2nd System): %v", err)
	}
	if res.Valid {
		t.Errorf("ObjectStoreValidate(2nd System): want deny, got allow")
	}

	// A non-System cluster -> allow.
	res, err = v.ObjectStoreValidate(context.Background(), nil, oscObj("lw", "Lightweight"))
	if err != nil {
		t.Fatalf("ObjectStoreValidate(Lightweight): %v", err)
	}
	if !res.Valid {
		t.Errorf("ObjectStoreValidate(Lightweight): want allow, got deny (%s)", res.Message)
	}
}
