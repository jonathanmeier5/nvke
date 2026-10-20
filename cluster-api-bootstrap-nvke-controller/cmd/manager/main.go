// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command manager runs the NVKE bootstrap provider.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bootstrapv1 "github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/api/v1alpha1"
	"github.com/dsx-ai-factory/nvke/cluster-api-bootstrap-nvke-controller/internal/controllers"
)

var scheme = runtime.NewScheme() //nolint:gochecknoglobals // the manager needs one scheme for the process.

func init() { //nolint:gochecknoinits // scheme registration is the controller-runtime convention.
	utilruntime(clientgoscheme.AddToScheme(scheme))
	utilruntime(clusterv1.AddToScheme(scheme))
	utilruntime(bootstrapv1.AddToScheme(scheme))
}

func utilruntime(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var spireServerAddress, spireServerPort, spireTrustDomain, spireTrustBundleFile string
	var nodeAgentPort string
	var disableMTLS, containerNodes bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for the manager.")
	flag.StringVar(&spireServerAddress, "spire-server-address", "", "SPIRE server address written into the node's spire-agent configuration.")
	flag.StringVar(&spireServerPort, "spire-server-port", "8081", "SPIRE server port written into the node's spire-agent configuration.")
	flag.StringVar(&spireTrustDomain, "spire-trust-domain", "", "SPIRE trust domain written into the node's spire-agent configuration.")
	flag.StringVar(&spireTrustBundleFile, "spire-trust-bundle-file", "", "File holding the SPIRE trust bundle to place on each node.")
	flag.StringVar(&nodeAgentPort, "node-agent-port", "", "nvke-agent --secure-server-port. Empty uses the agent's own default.")
	flag.BoolVar(&disableMTLS, "disable-mtls", false, "Serve the node agent's gRPC in plaintext and leave SPIRE out of the node configuration. Development only.")
	flag.BoolVar(&containerNodes, "container-nodes", false, "The hosts are nico-sim containers: keep NKE_LOCAL_DEV and --disable-mlx5-rescan in the agent environment.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "nvke-bootstrap.cluster.x-k8s.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Refusing to start beats rendering a node configuration with no SPIRE in
	// it: that node's agent would block in NewX509Source and never listen, with
	// no error anywhere to explain it.
	var trustBundle string
	if !disableMTLS {
		if spireServerAddress == "" || spireTrustDomain == "" || spireTrustBundleFile == "" {
			setupLog.Error(nil, "--spire-server-address, --spire-trust-domain and --spire-trust-bundle-file are required unless --disable-mtls is set")
			os.Exit(1)
		}
		raw, readErr := os.ReadFile(spireTrustBundleFile)
		if readErr != nil {
			setupLog.Error(readErr, "unable to read the SPIRE trust bundle", "path", spireTrustBundleFile)
			os.Exit(1)
		}
		trustBundle = string(raw)
	}

	if err := (&controllers.NvkeConfigReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		SpireServerAddress: spireServerAddress,
		SpireServerPort:    spireServerPort,
		SpireTrustDomain:   spireTrustDomain,
		SpireTrustBundle:   trustBundle,
		NodeAgentPort:      nodeAgentPort,
		DisableMTLS:        disableMTLS,
		ContainerNodes:     containerNodes,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NvkeConfig")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
