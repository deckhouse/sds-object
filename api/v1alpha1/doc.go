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

// Package v1alpha1 contains API Schema definitions for the storage.deckhouse.io
// resources managed by the sds-object module.
//
//   - ObjectStore — cluster-scoped CR describing an S3-compatible
//     object storage instance (one of four turnkey profiles); deploys the
//     data plane (outside the COSI model).
//   - Bucket — cluster-scoped CR representing a single backing bucket,
//     either administrator-declared (Shared) or provisioned for a BucketClaim.
//   - BucketClaim — namespaced CR requesting a bucket: greenfield
//     (provisions its own private bucket) or brownfield (binds a Shared
//     bucket, gated by BucketClaimPolicy).
//   - BucketAccess — namespaced CR requesting scoped credentials for a
//     BucketClaim (writes an S3 credentials Secret).
//   - BucketClaimPolicy — cluster-scoped CR gating which namespaces may bind a
//     Shared bucket via a brownfield BucketClaim.
//
// +groupName=storage.deckhouse.io
// +k8s:deepcopy-gen=package
package v1alpha1
