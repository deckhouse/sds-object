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
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/dynamic"
	clientgokube "k8s.io/client-go/kubernetes"
)

// anySpecFailed records whether any spec failed during the run. cleanupSuite
// consults it together with E2E_KEEP_CLUSTER_ON_FAILURE to decide whether to
// skip nested-cluster teardown.
var anySpecFailed bool

var _ = BeforeSuite(func() {
	prepareSuite()
})

var _ = AfterSuite(func() {
	cleanupSuite()
})

func TestSdsObject(t *testing.T) {
	RegisterFailHandler(Fail)

	suiteConfig, reporterConfig := GinkgoConfiguration()
	if os.Getenv("CI") != "" {
		suiteConfig.FailFast = true
		// Generous: the Heavy profile brings up a full Rook Ceph cluster (via an
		// sds-elastic ElasticCluster) on top of the Full (SeaweedFS + Postgres) and
		// Garage profiles, so the whole suite can run well past an hour.
		suiteConfig.Timeout = 180 * time.Minute
	}
	// The suite shares one ObjectStorageCluster across dependency-ordered specs
	// (create -> bucket + S3 round-trip -> validation guards -> delete), so spec
	// randomization MUST stay OFF.
	suiteConfig.RandomizeAllSpecs = false
	reporterConfig.Verbose = true
	reporterConfig.ShowNodeEvents = false

	RunSpecs(t, "sds-object E2E Suite", suiteConfig, reporterConfig)
}

// The single root Ordered container. Spec registration goes through builder
// functions called in EXPLICIT dependency order: per-file top-level Describes
// would order alphabetically and break the create-before-delete invariant.
var _ = Describe("sds-object e2e", Ordered, ContinueOnFailure, func() {
	BeforeAll(prepareSharedState)

	// Dump OSC/OB conditions, module pods and events on any failure.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		anySpecFailed = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		dumpFailedSpecDiagnostics(ctx)
	})

	createSpecs()             // create_test.go: OSC -> Ready, OB -> Ready, creds Secret, S3 round-trip
	validationSpecs()         // validation_test.go: webhook + CEL admission guards
	accessSpecs()             // access_test.go: deny-by-default + revocation, regexp policy, key rotation, ReadOnly, cross-namespace
	systemBucketSpecs()       // system_test.go: built-in system OSCluster+OSB+policy shipped by templates
	lightweightSpecs()        // lightweight_test.go: Lightweight (Garage on PVC) create -> bucket -> round-trip -> delete
	fullSpecs()               // full_test.go: Full (SeaweedFS, Single/leveldb) create -> bucket -> round-trip -> delete
	fullHighRedundancySpecs() // full_test.go: Full HighRedundancy (SeaweedFS multi-filer HA + managed-postgres)
	heavySpecs()              // heavy_test.go: Heavy (Ceph RGW on sds-elastic ElasticCluster) bring-up -> create -> bucket -> round-trip -> delete
	deleteSpecs()             // delete_test.go: OB delete (+ creds Secret + reclaim), OSC delete
})

func prepareSuite() {
	suiteCfg = loadConfig()

	GinkgoWriter.Printf("E2E config:\n")
	GinkgoWriter.Printf("  TEST_CLUSTER_CREATE_MODE: %q\n", os.Getenv("TEST_CLUSTER_CREATE_MODE"))
	GinkgoWriter.Printf("  namespace (TEST_CLUSTER_NAMESPACE): %q\n", suiteCfg.namespace)
	GinkgoWriter.Printf("  OSC name / type / redundancy: %q / %q / %q\n", suiteCfg.oscName, suiteCfg.oscType, suiteCfg.redundancy)
	if suiteCfg.needsStorageClass() {
		GinkgoWriter.Printf("  storage class / size: %q / %q\n", suiteCfg.storageCl, suiteCfg.oscSize)
	}
	if suiteCfg.isHeavy() {
		GinkgoWriter.Printf("  elasticClusterRef: %q\n", suiteCfg.elasticRef)
	}
	GinkgoWriter.Printf("  bucket name: %q\n", suiteCfg.bucketName)
	GinkgoWriter.Printf("  module ready timeout: %s\n", suiteCfg.moduleReadyTO)
	GinkgoWriter.Printf("  OSC ready timeout: %s\n", suiteCfg.oscReadyTimeout)
	GinkgoWriter.Printf("  OB ready timeout: %s\n", suiteCfg.obReadyTimeout)
	GinkgoWriter.Printf("  probe image: %q\n", suiteCfg.probeImage)

	// Fail fast on misconfigured heavier profiles rather than after a 15m wait.
	if suiteCfg.needsStorageClass() && suiteCfg.storageCl == "" {
		Fail("E2E_OSC_TYPE=" + suiteCfg.oscType + " requires a StorageClass; set E2E_STORAGE_CLASS")
	}
	if suiteCfg.isHeavy() && suiteCfg.elasticRef == "" {
		Fail("E2E_OSC_TYPE=Heavy requires E2E_ELASTIC_CLUSTER_REF (a Ready ElasticCluster)")
	}

	ensureNestedTestCluster()

	var err error
	suiteRestCfg = suiteClusterResources.Kubeconfig

	suiteClientset, err = clientgokube.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build clientset")

	suiteDyn, err = dynamic.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build dynamic client")

	ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.moduleReadyTO+5*time.Minute)
	defer cancel()

	By("Waiting for the sds-object module to become Ready")
	Expect(waitModuleReady(ctx)).To(Succeed(), "sds-object module readiness")

	By("Waiting for the sds-object controller (validating webhook) to be Ready")
	Expect(waitControllerReady(ctx, suiteCfg.moduleReadyTO)).To(Succeed(), "sds-object controller/webhook readiness")

	By("Ensuring the in-cluster test namespace exists")
	Expect(ensureNamespace(ctx, suiteCfg.namespace)).To(Succeed())
}

// prepareSharedState runs once before the Ordered specs. Clients and module
// readiness are already set up in BeforeSuite; this is the hook where specs wire
// any additional shared fixtures.
func prepareSharedState() {
	GinkgoWriter.Printf("Shared ObjectStorageCluster for this run: %s (type %s, namespace %s)\n", suiteCfg.oscName, suiteCfg.oscType, suiteCfg.namespace)
}

func cleanupSuite() {
	// Keep the nested cluster alive for manual debugging when a spec failed and
	// the operator asked for it. Otherwise tear it down (the only mandatory step;
	// resource-level cleanup is driven by the specs themselves).
	if suiteCfg.keepClusterOnFailure && anySpecFailed {
		printKeepClusterBanner()
		return
	}
	cleanupNestedTestCluster()
}

func printKeepClusterBanner() {
	GinkgoWriter.Printf("\n========== E2E_KEEP_CLUSTER_ON_FAILURE: cluster preserved ==========\n")
	GinkgoWriter.Printf("A spec failed and nested-cluster teardown was SKIPPED for debugging.\n")
	GinkgoWriter.Printf("  namespace (OB/Secret + base VM ns): %s\n", suiteCfg.namespace)
	GinkgoWriter.Printf("  ObjectStorageCluster:               %s (type %s)\n", suiteCfg.oscName, suiteCfg.oscType)
	GinkgoWriter.Printf("  module namespace:                   %s\n", moduleNS)
	if suiteClusterResources != nil && suiteClusterResources.KubeconfigPath != "" {
		GinkgoWriter.Printf("  kubeconfig (export KUBECONFIG):     %s\n", suiteClusterResources.KubeconfigPath)
	}
	GinkgoWriter.Printf("Remember to delete the VMs / nested cluster manually when finished.\n")
	GinkgoWriter.Printf("====================================================================\n")
}
