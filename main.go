/*
Copyright 2024.

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
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/fernet/fernet-go"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/pbkdf2"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	proxyv1 "livellm-proxy-operator/api/v1"
	"livellm-proxy-operator/controllers"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(proxyv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ── Redis (required) ──────────────────────────────────────────────────────
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		setupLog.Error(fmt.Errorf("REDIS_URL environment variable is required"), "missing configuration")
		os.Exit(1)
	}

	redisOpts, err := goredis.ParseURL(redisURL)
	if err != nil {
		setupLog.Error(err, "Failed to parse REDIS_URL")
		os.Exit(1)
	}

	redisClient := goredis.NewClient(redisOpts)
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		setupLog.Error(err, "Failed to connect to Redis")
		os.Exit(1)
	}
	setupLog.Info("Connected to Redis successfully")

	// ── Optional Fernet encryption (ENCRYPTION_SALT) ─────────────────────────
	// Key derivation matches the Python proxy exactly:
	//   PBKDF2-HMAC-SHA256(password="livellm-proxy-key", salt=ENCRYPTION_SALT,
	//                       iterations=100000, dklen=32)
	//   → base64url-encode → Fernet key
	var encryptionKey *fernet.Key
	if salt := os.Getenv("ENCRYPTION_SALT"); salt != "" {
		derived := pbkdf2.Key([]byte("livellm-proxy-key"), []byte(salt), 100000, 32, sha256.New)
		b64Key := base64.URLEncoding.EncodeToString(derived)
		encryptionKey, err = fernet.DecodeKey(b64Key)
		if err != nil {
			setupLog.Error(err, "Failed to derive Fernet key from ENCRYPTION_SALT")
			os.Exit(1)
		}
		setupLog.Info("Fernet encryption enabled (ENCRYPTION_SALT is set)")
	} else {
		setupLog.Info("ENCRYPTION_SALT not set — provider data will be stored unencrypted")
	}

	// ── Controller manager ────────────────────────────────────────────────────
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "livellm-proxy-operator.livellm.ai",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.LLMProviderReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		RedisClient:   redisClient,
		EncryptionKey: encryptionKey,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LLMProvider")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

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
