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
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
	kwhlogrus "github.com/slok/kubewebhook/v2/pkg/log/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/deckhouse/sds-object/images/webhooks/handlers"
)

type config struct {
	certFile string
	keyFile  string
}

func httpHandlerHealthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, "Ok.")
}

func initFlags() (config, error) {
	cfg := config{}

	fl := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fl.StringVar(&cfg.certFile, "tls-cert-file", "", "TLS certificate file")
	fl.StringVar(&cfg.keyFile, "tls-key-file", "", "TLS key file")

	err := fl.Parse(os.Args[1:])
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

const (
	port = ":8443"

	ObjectStoreValidatorID  = "ObjectStoreValidator"
	BucketValidatorID       = "BucketValidator"
	BucketClaimValidatorID  = "BucketClaimValidator"
	BucketAccessValidatorID = "BucketAccessValidator"
	BucketPolicyValidatorID = "BucketPolicyValidator"
)

func main() {
	logrusLogEntry := logrus.NewEntry(logrus.New())
	logrusLogEntry.Logger.SetLevel(logrus.DebugLevel)
	logger := kwhlogrus.NewLogrus(logrusLogEntry)

	cfg, err := initFlags()
	if err != nil {
		fmt.Printf("unable to parse config: err: %s", err.Error())
		os.Exit(1)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to build in-cluster config: %s", err)
		os.Exit(1)
	}
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to build dynamic client: %s", err)
		os.Exit(1)
	}
	validator := handlers.NewValidator(dynClient)

	oscValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		validator.ObjectStoreValidate,
		ObjectStoreValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating oscValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	osbValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		validator.BucketValidate,
		BucketValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating osbValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	bucketClaimValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		validator.BucketClaimValidate,
		BucketClaimValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating bucketClaimValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	osbaValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		validator.BucketAccessValidate,
		BucketAccessValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating osbaValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	osbpValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		validator.BucketPolicyValidate,
		BucketPolicyValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating osbpValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/objectstore-validate", oscValidatingWebhookHandler)
	mux.Handle("/bucket-validate", osbValidatingWebhookHandler)
	mux.Handle("/bucketclaim-validate", bucketClaimValidatingWebhookHandler)
	mux.Handle("/bucketaccess-validate", osbaValidatingWebhookHandler)
	mux.Handle("/bucketpolicy-validate", osbpValidatingWebhookHandler)
	mux.HandleFunc("/healthz", httpHandlerHealthz)

	logger.Infof("Listening on %s", port)
	err = http.ListenAndServeTLS(port, cfg.certFile, cfg.keyFile, mux)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error serving webhook: %s", err)
		os.Exit(1)
	}
}
