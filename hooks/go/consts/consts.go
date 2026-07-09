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

package consts

const (
	ModuleName       string = "sdsObject"
	ModuleNamespace  string = "d8-sds-object"
	ModulePluralName string = "sds-object"
	// WebhookCertCn is the CN used for the self-signed TLS material that
	// the webhooks container consumes. It must match the Service name
	// (helm_lib_module_webhook_service defaults to "webhooks").
	WebhookCertCn string = "webhooks"
)

// AllowedProvisioners lists provisioners for which the module-delete hook
// should strip finalizers from StorageClass objects. Empty by default; add
// the sds-object provisioner(s) here once they exist.
var AllowedProvisioners = []string{}

// WebhookConfigurationsToDelete lists ValidatingWebhookConfigurations the
// module-delete hook removes on uninstall.
var WebhookConfigurationsToDelete = []string{
	"d8-sds-object-objectstore-validation",
	"d8-sds-object-bucket-validation",
	"d8-sds-object-bucketclaim-validation",
	"d8-sds-object-bucketaccess-validation",
	"d8-sds-object-bucketclaimpolicy-validation",
}

// CRGVKsForFinalizerRemoval lists CRs the module creates and which may carry a
// controller-managed finalizer, stripped on module delete.
var CRGVKsForFinalizerRemoval = []CRGVK{
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "ObjectStore", Namespaced: false},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "Bucket", Namespaced: false},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "BucketClaim", Namespaced: true},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "BucketAccess", Namespaced: true},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "BucketClaimPolicy", Namespaced: false},
}

type CRGVK struct {
	Group      string
	Version    string
	Kind       string
	Namespaced bool
}
