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

package config

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/deckhouse/sds-object/images/controller/pkg/logger"
)

const (
	LogLevelEnv                = "LOG_LEVEL"
	ControllerNamespaceEnv     = "CONTROLLER_NAMESPACE"
	HealthProbeBindAddressEnv  = "HEALTH_PROBE_BIND_ADDRESS"
	MaxConcurrentReconcilesEnv = "MAX_CONCURRENT_RECONCILES"
	RequeueIntervalEnv         = "REQUEUE_INTERVAL_SECONDS"
	GarageImageEnv             = "GARAGE_IMAGE"
	ClusterDomainEnv           = "CLUSTER_DOMAIN"

	DefaultControllerNamespace     = "d8-sds-object"
	DefaultControllerName          = "sds-object-controller"
	DefaultHealthProbeBindAddress  = ":8081"
	DefaultRequeueIntervalSeconds  = 30
	DefaultMaxConcurrentReconciles = 1
	DefaultClusterDomain           = "cluster.local"
)

type Options struct {
	Loglevel                logger.Verbosity
	HealthProbeBindAddress  string
	ControllerNamespace     string
	MaxConcurrentReconciles int
	RequeueInterval         time.Duration
	// GarageImage is the module registry reference for the Garage server
	// image, injected via the GARAGE_IMAGE env var from Helm.
	GarageImage string
	// ClusterDomain is the Kubernetes cluster DNS domain (e.g. cluster.local),
	// injected via CLUSTER_DOMAIN from global.discovery.clusterDomain. Used to
	// build in-cluster Service FQDNs.
	ClusterDomain string
}

func NewConfig() *Options {
	var opts Options

	if v := os.Getenv(LogLevelEnv); v != "" {
		opts.Loglevel = logger.Verbosity(v)
	} else {
		opts.Loglevel = logger.DebugLevel
	}

	if v := os.Getenv(HealthProbeBindAddressEnv); v != "" {
		opts.HealthProbeBindAddress = v
	} else {
		opts.HealthProbeBindAddress = DefaultHealthProbeBindAddress
	}

	opts.ControllerNamespace = os.Getenv(ControllerNamespaceEnv)
	if opts.ControllerNamespace == "" {
		if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			opts.ControllerNamespace = string(ns)
		} else {
			log.Printf("Failed to read namespace from filesystem: %v; falling back to %q", err, DefaultControllerNamespace)
			opts.ControllerNamespace = DefaultControllerNamespace
		}
	}

	opts.MaxConcurrentReconciles = DefaultMaxConcurrentReconciles
	if v := os.Getenv(MaxConcurrentReconcilesEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.MaxConcurrentReconciles = n
		}
	}

	opts.RequeueInterval = time.Duration(DefaultRequeueIntervalSeconds) * time.Second
	if v := os.Getenv(RequeueIntervalEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.RequeueInterval = time.Duration(n) * time.Second
		}
	}

	opts.GarageImage = os.Getenv(GarageImageEnv)

	opts.ClusterDomain = os.Getenv(ClusterDomainEnv)
	if opts.ClusterDomain == "" {
		opts.ClusterDomain = DefaultClusterDomain
	}

	return &opts
}
