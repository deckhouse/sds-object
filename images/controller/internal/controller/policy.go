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
	"context"
	"fmt"
	"regexp"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
)

// namespaceAllowedForBucket reports whether the given namespace is permitted to
// request access to the bucket. Access is deny-by-default: it returns true only
// when at least one BucketClaimPolicy for the bucket matches the
// namespace (by exact name or by a regexp pattern). The returned string
// explains the decision for surfacing in the access status.
//
// It takes a client.Reader so callers can pass an uncached APIReader: this is a
// security-boundary decision, and reading policies through the informer cache
// risks a TOCTOU where a just-deleted/edited policy still grants access. Pass
// mgr.GetAPIReader() to read the authoritative state.
func namespaceAllowedForBucket(ctx context.Context, c client.Reader, bucketName, namespace string) (bool, string, error) {
	list := &v1alpha1.BucketClaimPolicyList{}
	if err := c.List(ctx, list); err != nil {
		return false, "", fmt.Errorf("list BucketPolicies: %w", err)
	}

	policies := 0
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.BucketRef != bucketName {
			continue
		}
		policies++
		if match, err := namespaceMatches(p.Spec.AllowedNamespaces, namespace); err != nil {
			return false, "", err
		} else if match {
			return true, fmt.Sprintf("allowed by BucketClaimPolicy %q", p.Name), nil
		}
	}

	if policies == 0 {
		return false, fmt.Sprintf("no BucketClaimPolicy grants namespace %q access to bucket %q (access is deny-by-default)", namespace, bucketName), nil
	}
	return false, fmt.Sprintf("namespace %q does not match any BucketClaimPolicy for bucket %q", namespace, bucketName), nil
}

// namespaceMatches reports whether the namespace is selected by the match: it
// appears in Names, or fully matches one of the RE2 Patterns.
func namespaceMatches(match v1alpha1.NamespaceMatch, namespace string) (bool, error) {
	for _, n := range match.Names {
		if n == namespace {
			return true, nil
		}
	}
	for _, pat := range match.Patterns {
		re, err := regexp.Compile(anchor(pat))
		if err != nil {
			return false, fmt.Errorf("invalid namespace pattern %q: %w", pat, err)
		}
		if re.MatchString(namespace) {
			return true, nil
		}
	}
	return false, nil
}

// anchor wraps a pattern so it must match the whole namespace name.
func anchor(pattern string) string {
	return "^(?:" + pattern + ")$"
}
