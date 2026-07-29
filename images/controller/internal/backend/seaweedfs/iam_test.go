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
	"reflect"
	"testing"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

func TestBucketActions(t *testing.T) {
	got := bucketActions("media", v1alpha1.AccessReadWrite)
	want := []string{"Read:media", "Write:media", "List:media", "Tagging:media"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bucketActions=%v, want %v", got, want)
	}
}
