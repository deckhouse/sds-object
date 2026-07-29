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
	SecurityResyncIntervalEnv  = "SECURITY_RESYNC_INTERVAL_SECONDS"
	ReconcileTimeoutEnv        = "RECONCILE_TIMEOUT_SECONDS"
	GarageImageEnv             = "GARAGE_IMAGE"
	SeaweedFSImageEnv          = "SEAWEEDFS_IMAGE"
	ClusterDomainEnv           = "CLUSTER_DOMAIN"

	DefaultControllerNamespace     = "d8-sds-object"
	DefaultControllerName          = "sds-object-controller"
	DefaultHealthProbeBindAddress  = ":8081"
	DefaultRequeueIntervalSeconds  = 30
	DefaultMaxConcurrentReconciles = 1
	DefaultClusterDomain           = "cluster.local"
	// DefaultSecurityResyncIntervalSeconds bounds how long an already-Ready
	// BucketClaim/BucketAccess can carry a stale authorization (e.g. a dangling
	// grant after a missed watch event) before the reconciler re-drives it and
	// re-checks the policy. Cheap safety net under the watch chain (300s = 5m).
	DefaultSecurityResyncIntervalSeconds = 300
	// DefaultReconcileTimeoutSeconds bounds a single reconcile. Reconcilers talk
	// to backends over HTTP, exec into data-plane pods and wait on the API
	// server; a reconcile context carries no deadline of its own, so one call
	// that never returns takes its worker with it — and with
	// MaxConcurrentReconciles at 1 that is the whole controller, silently, until
	// the pod is restarted. A deadline turns that into an error, a log line
	// naming the object, and a retry.
	DefaultReconcileTimeoutSeconds = 300
)

type Options struct {
	Loglevel                logger.Verbosity
	HealthProbeBindAddress  string
	ControllerNamespace     string
	MaxConcurrentReconciles int
	RequeueInterval         time.Duration
	// SecurityResyncInterval is the requeue cadence for already-Ready
	// BucketClaim/BucketAccess objects, so a missed watch event on the
	// deny-by-default revocation chain self-heals within minutes instead of
	// waiting for the ~10h informer resync.
	SecurityResyncInterval time.Duration
	// ReconcileTimeout bounds a single Reconcile call, so a backend that stops
	// answering cannot wedge a controller worker indefinitely.
	ReconcileTimeout time.Duration
	// GarageImage is the module registry reference for the Garage server
	// image, injected via the GARAGE_IMAGE env var from Helm.
	GarageImage string
	// SeaweedFSImage is the module registry reference for the SeaweedFS server
	// image, injected via the SEAWEEDFS_IMAGE env var from Helm.
	SeaweedFSImage string
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
		opts.Loglevel = logger.InfoLevel
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

	opts.SecurityResyncInterval = time.Duration(DefaultSecurityResyncIntervalSeconds) * time.Second
	if v := os.Getenv(SecurityResyncIntervalEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.SecurityResyncInterval = time.Duration(n) * time.Second
		}
	}

	opts.ReconcileTimeout = time.Duration(DefaultReconcileTimeoutSeconds) * time.Second
	if v := os.Getenv(ReconcileTimeoutEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.ReconcileTimeout = time.Duration(n) * time.Second
		}
	}

	opts.GarageImage = os.Getenv(GarageImageEnv)
	opts.SeaweedFSImage = os.Getenv(SeaweedFSImageEnv)

	opts.ClusterDomain = os.Getenv(ClusterDomainEnv)
	if opts.ClusterDomain == "" {
		opts.ClusterDomain = DefaultClusterDomain
	}

	return &opts
}
