/*
Copyright 2026.

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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fernet/fernet-go"
	goredis "github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	proxyv1 "livellm-proxy-operator/api/v1"
)

const (
	llmProviderFinalizer  = "llmprovider.proxy.livellm.ai/finalizer"
	redisProvidersKey     = "livellm:providers"
	redisProvidersChannel = "livellm:providers:events"
)

// LLMProviderReconciler reconciles a LLMProvider object
type LLMProviderReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	RedisClient   *goredis.Client
	EncryptionKey *fernet.Key // nil when ENCRYPTION_SALT is not configured
}

// +kubebuilder:rbac:groups=proxy.livellm.ai,resources=llmproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxy.livellm.ai,resources=llmproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxy.livellm.ai,resources=llmproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *LLMProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	llmProvider := &proxyv1.LLMProvider{}
	err := r.Get(ctx, req.NamespacedName, llmProvider)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion via finalizer
	if !llmProvider.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(llmProvider, llmProviderFinalizer) {
			uid := fmt.Sprintf("%s-%s", llmProvider.Namespace, llmProvider.Name)
			if err := r.deleteFromRedis(ctx, uid); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(llmProvider, llmProviderFinalizer)
			if err := r.Update(ctx, llmProvider); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(llmProvider, llmProviderFinalizer) {
		controllerutil.AddFinalizer(llmProvider, llmProviderFinalizer)
		if err := r.Update(ctx, llmProvider); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	uid := fmt.Sprintf("%s-%s", llmProvider.Namespace, llmProvider.Name)

	// Decide whether we need to write to Redis
	shouldRegister := false
	if llmProvider.Generation != llmProvider.Status.LastSyncedGeneration {
		shouldRegister = true
	}

	if !shouldRegister {
		exists, err := r.RedisClient.HExists(ctx, redisProvidersKey, uid).Result()
		if err != nil {
			log.Error(err, "Failed to check provider existence in Redis")
			shouldRegister = true
		} else if !exists {
			log.Info("Provider missing in Redis, triggering registration", "uid", uid)
			shouldRegister = true
		}
	}

	if shouldRegister {
		if err := r.registerToRedis(ctx, llmProvider, uid); err != nil {
			log.Error(err, "Failed to register provider to Redis")

			if refreshErr := r.Get(ctx, req.NamespacedName, llmProvider); refreshErr != nil {
				return ctrl.Result{}, refreshErr
			}
			llmProvider.Status.Registered = false
			llmProvider.Status.Message = err.Error()
			if updateErr := r.Status().Update(ctx, llmProvider); updateErr != nil {
				if errors.IsConflict(updateErr) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		if refreshErr := r.Get(ctx, req.NamespacedName, llmProvider); refreshErr != nil {
			return ctrl.Result{}, refreshErr
		}
		llmProvider.Status.LastSyncedGeneration = llmProvider.Generation
		llmProvider.Status.Registered = true
		llmProvider.Status.Message = "Successfully registered"
		if updateErr := r.Status().Update(ctx, llmProvider); updateErr != nil {
			if errors.IsConflict(updateErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, updateErr
		}
	} else if !llmProvider.Status.Registered {
		if refreshErr := r.Get(ctx, req.NamespacedName, llmProvider); refreshErr != nil {
			return ctrl.Result{}, refreshErr
		}
		llmProvider.Status.Registered = true
		llmProvider.Status.Message = "Successfully registered"
		if updateErr := r.Status().Update(ctx, llmProvider); updateErr != nil {
			if errors.IsConflict(updateErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, updateErr
		}
	}

	reconcileInterval := 5 * time.Minute
	if llmProvider.Spec.RefreshInterval != "" {
		if d, err := time.ParseDuration(llmProvider.Spec.RefreshInterval); err == nil {
			reconcileInterval = d
		} else {
			log.Error(err, "Invalid refreshInterval, using default 5m")
		}
	}

	return ctrl.Result{RequeueAfter: reconcileInterval}, nil
}

// registerToRedis writes an encrypted (or plain) provider JSON blob to the Redis
// hash and publishes a Pub/Sub event so all proxy replicas hot-reload.
func (r *LLMProviderReconciler) registerToRedis(ctx context.Context, provider *proxyv1.LLMProvider, uid string) error {
	log := log.FromContext(ctx)

	// Fetch the API key from the referenced K8s Secret
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      provider.Spec.APIKeyRef.Name,
		Namespace: provider.Namespace,
	}, secret); err != nil {
		return fmt.Errorf("failed to get secret %s: %w", provider.Spec.APIKeyRef.Name, err)
	}

	apiKeyBytes, ok := secret.Data[provider.Spec.APIKeyRef.Key]
	if !ok {
		return fmt.Errorf("key %s not found in secret %s", provider.Spec.APIKeyRef.Key, provider.Spec.APIKeyRef.Name)
	}

	// Build the provider payload — must match the Python Settings model fields
	payload := map[string]interface{}{
		"uid":      uid,
        "provider": provider.Spec.Provider,
        "api_key":  string(apiKeyBytes),
    }
    if provider.Spec.BaseURL != "" {
        payload["base_url"] = provider.Spec.BaseURL
    }
    if len(provider.Spec.BlacklistModels) > 0 {
        payload["blacklist_models"] = provider.Spec.BlacklistModels
    }
    if len(provider.Spec.ModelConfigs) > 0 {
        payload["model_configs"] = provider.Spec.ModelConfigs
    }

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal provider payload: %w", err)
	}

	// Optionally encrypt with Fernet (key derivation matches the Python proxy exactly)
	var data []byte
	if r.EncryptionKey != nil {
		token, err := fernet.EncryptAndSign(jsonBytes, r.EncryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt provider data: %w", err)
		}
		data = token
	} else {
		data = jsonBytes
	}

	// Persist to the shared Redis hash
	if err := r.RedisClient.HSet(ctx, redisProvidersKey, uid, data).Err(); err != nil {
		return fmt.Errorf("failed to write provider to Redis: %w", err)
	}

	// Notify all proxy replicas via Pub/Sub (non-fatal if it fails)
	eventMsg, _ := json.Marshal(map[string]string{"action": "upsert", "uid": uid})
	if err := r.RedisClient.Publish(ctx, redisProvidersChannel, string(eventMsg)).Err(); err != nil {
		log.Error(err, "Failed to publish upsert event — replicas will sync on next reconcile")
	}

	log.Info("Provider registered to Redis", "uid", uid, "encrypted", r.EncryptionKey != nil)
	return nil
}

// deleteFromRedis removes the provider from Redis and notifies all proxy replicas.
func (r *LLMProviderReconciler) deleteFromRedis(ctx context.Context, uid string) error {
	log := log.FromContext(ctx)

	if err := r.RedisClient.HDel(ctx, redisProvidersKey, uid).Err(); err != nil {
		return fmt.Errorf("failed to delete provider from Redis: %w", err)
	}

	eventMsg, _ := json.Marshal(map[string]string{"action": "delete", "uid": uid})
	if err := r.RedisClient.Publish(ctx, redisProvidersChannel, string(eventMsg)).Err(); err != nil {
		log.Error(err, "Failed to publish delete event — replicas will sync on next reconcile")
	}

	log.Info("Provider deleted from Redis", "uid", uid)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LLMProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1.LLMProvider{}).
		Complete(r)
}
