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

package main

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"

	v1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/deckhouse/sds-object/api/v1alpha1"
	"github.com/deckhouse/sds-object/images/controller/internal/backend"
	"github.com/deckhouse/sds-object/images/controller/internal/backend/garage"
	"github.com/deckhouse/sds-object/images/controller/internal/controller"
	"github.com/deckhouse/sds-object/images/controller/pkg/config"
	"github.com/deckhouse/sds-object/images/controller/pkg/kubutils"
	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

var (
	buildDate = ""
	version   = ""
	commit    = ""
)

var resourcesSchemeFuncs = []func(*apiruntime.Scheme) error{
	v1alpha1.AddToScheme,
	clientgoscheme.AddToScheme,
	v1.AddToScheme,
}

func main() {
	ctx := context.Background()
	cfgParams := config.NewConfig()

	log, err := logger.NewLogger(cfgParams.Loglevel)
	if err != nil {
		fmt.Printf("unable to create NewLogger, err: %v\n", err)
		os.Exit(1)
	}

	log.Info(fmt.Sprintf("[main] sds-object-controller version=%s commit=%s buildDate=%s", version, commit, buildDate))
	log.Info(fmt.Sprintf("[main] Go Version: %s", goruntime.Version()))
	log.Info(fmt.Sprintf("[main] OS/Arch: %s/%s", goruntime.GOOS, goruntime.GOARCH))
	log.Info(fmt.Sprintf("[main] ControllerNamespace=%s", cfgParams.ControllerNamespace))
	log.Info(fmt.Sprintf("[main] MaxConcurrentReconciles=%d", cfgParams.MaxConcurrentReconciles))
	log.Info(fmt.Sprintf("[main] RequeueInterval=%s", cfgParams.RequeueInterval))

	kConfig, err := kubutils.KubernetesDefaultConfigCreate()
	if err != nil {
		log.Error(err, "[main] unable to create kubernetes config")
		os.Exit(1)
	}

	scheme := apiruntime.NewScheme()
	for _, f := range resourcesSchemeFuncs {
		if err := f(scheme); err != nil {
			log.Error(err, "[main] unable to register scheme")
			os.Exit(1)
		}
	}

	managerOpts := manager.Options{
		Scheme:                  scheme,
		HealthProbeBindAddress:  cfgParams.HealthProbeBindAddress,
		LeaderElection:          true,
		LeaderElectionNamespace: cfgParams.ControllerNamespace,
		LeaderElectionID:        config.DefaultControllerName,
		Logger:                  log.GetLogger(),
	}

	mgr, err := manager.New(kConfig, managerOpts)
	if err != nil {
		log.Error(err, "[main] unable to create manager")
		os.Exit(1)
	}
	log.Info("[main] kubernetes manager created")

	// Garage backs the System and Lightweight profiles; SeaweedFS (Full) and
	// Ceph RGW (Heavy) are still stubbed (NotImplementedDriver) until their
	// drivers land.
	registry := backend.NewRegistry(
		garage.New(mgr.GetClient(), log, cfgParams.ControllerNamespace, cfgParams.GarageImage),
		backend.NotImplementedDriver{BackendType: v1alpha1.BackendSeaweedFS},
		backend.NotImplementedDriver{BackendType: v1alpha1.BackendCephRGW},
	)

	if err := controller.AddObjectStorageClusterReconcilerToManager(mgr, cfgParams, log, registry); err != nil {
		log.Error(err, "[main] unable to register ObjectStorageCluster reconciler")
		os.Exit(1)
	}
	if err := controller.AddObjectBucketReconcilerToManager(mgr, cfgParams, log, registry); err != nil {
		log.Error(err, "[main] unable to register ObjectBucket reconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to AddHealthzCheck")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to AddReadyzCheck")
		os.Exit(1)
	}

	log.Info("[main] starting manager")
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "[main] manager exited with error")
		os.Exit(1)
	}
}
