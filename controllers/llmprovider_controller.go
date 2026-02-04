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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
	llmProviderFinalizer           = "llmprovider.proxy.livellm.ai/finalizer"
	lastSyncedGenerationAnnotation = "proxy.livellm.ai/last-synced-generation"
)

type ProxyConfig struct {
	UID string `json:"uid"`
}

// LLMProviderReconciler reconciles a LLMProvider object
type LLMProviderReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	ProxyURL string
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

	// Examine DeletionTimestamp to determine if object is under deletion
	if llmProvider.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// to registering our finalizer.
		if !controllerutil.ContainsFinalizer(llmProvider, llmProviderFinalizer) {
			controllerutil.AddFinalizer(llmProvider, llmProviderFinalizer)
			if err := r.Update(ctx, llmProvider); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(llmProvider, llmProviderFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteExternalResources(ctx, llmProvider); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(llmProvider, llmProviderFinalizer)
			if err := r.Update(ctx, llmProvider); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// Determine if we need to register/update
	shouldRegister := false
	currentGen := strconv.FormatInt(llmProvider.Generation, 10)

	// 1. Check if Spec has changed
	if llmProvider.Annotations == nil || llmProvider.Annotations[lastSyncedGenerationAnnotation] != currentGen {
		shouldRegister = true
	}

	// 2. If Spec hasn't changed, check if it exists in Proxy (Verification)
	if !shouldRegister {
		exists, err := r.checkProviderExists(ctx, llmProvider)
		if err != nil {
			log.Error(err, "Failed to check provider existence in proxy")
			// If check fails, we assume we might need to register to be safe,
			// or we could skip and retry later. Let's try to register.
			shouldRegister = true
		} else if !exists {
			log.Info("Provider missing in proxy, triggering registration")
			shouldRegister = true
		}
	}

	if shouldRegister {
		// Logic to register provider
		err = r.registerProvider(ctx, llmProvider)
		if err != nil {
			log.Error(err, "Failed to register provider")
			llmProvider.Status.Registered = false
			llmProvider.Status.Message = err.Error()
			if updateErr := r.Status().Update(ctx, llmProvider); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			// Requeue with delay to retry
			return ctrl.Result{RequeueAfter: time.Minute}, err
		}

		// Update Annotation to track sync state
		if llmProvider.Annotations == nil {
			llmProvider.Annotations = make(map[string]string)
		}
		llmProvider.Annotations[lastSyncedGenerationAnnotation] = currentGen
		if err := r.Update(ctx, llmProvider); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !llmProvider.Status.Registered {
		llmProvider.Status.Registered = true
		llmProvider.Status.Message = "Successfully registered"
		if err := r.Status().Update(ctx, llmProvider); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Periodic reconciliation
	reconcileInterval := 5 * time.Minute
	if llmProvider.Spec.RefreshInterval != "" {
		if d, err := time.ParseDuration(llmProvider.Spec.RefreshInterval); err == nil {
			reconcileInterval = d
		} else {
			log.Error(err, "Invalid refreshInterval format, using default 5m")
		}
	}

	return ctrl.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *LLMProviderReconciler) checkProviderExists(ctx context.Context, provider *proxyv1.LLMProvider) (bool, error) {
	url := fmt.Sprintf("%s/livellm/providers/configs", r.ProxyURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("proxy returned status %d", resp.StatusCode)
	}

	var configs []ProxyConfig
	if err := json.NewDecoder(resp.Body).Decode(&configs); err != nil {
		return false, err
	}

	targetUID := fmt.Sprintf("%s-%s", provider.Namespace, provider.Name)
	for _, config := range configs {
		if config.UID == targetUID {
			return true, nil
		}
	}

	return false, nil
}

func (r *LLMProviderReconciler) registerProvider(ctx context.Context, provider *proxyv1.LLMProvider) error {
	// Fetch API Key from Secret
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: provider.Spec.APIKeyRef.Name, Namespace: provider.Namespace}, secret)
	if err != nil {
		return fmt.Errorf("failed to get secret %s: %w", provider.Spec.APIKeyRef.Name, err)
	}

	apiKeyBytes, ok := secret.Data[provider.Spec.APIKeyRef.Key]
	if !ok {
		return fmt.Errorf("key %s not found in secret %s", provider.Spec.APIKeyRef.Key, provider.Spec.APIKeyRef.Name)
	}
	apiKey := string(apiKeyBytes)

	// Construct payload
	// Use namespace-name as UID to avoid collisions
	uid := fmt.Sprintf("%s-%s", provider.Namespace, provider.Name)

	payload := map[string]interface{}{
		"uid":      uid,
		"provider": provider.Spec.Provider,
		"api_key":  apiKey,
	}

	if provider.Spec.BaseURL != "" {
		payload["base_url"] = provider.Spec.BaseURL
	}
	if len(provider.Spec.BlacklistModels) > 0 {
		payload["blacklist_models"] = provider.Spec.BlacklistModels
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/livellm/providers/config", r.ProxyURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proxy returned status %d", resp.StatusCode)
	}

	return nil
}

func (r *LLMProviderReconciler) deleteExternalResources(ctx context.Context, provider *proxyv1.LLMProvider) error {
	uid := fmt.Sprintf("%s-%s", provider.Namespace, provider.Name)
	url := fmt.Sprintf("%s/livellm/providers/config/%s", r.ProxyURL, uid)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		// Already deleted
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proxy returned status %d", resp.StatusCode)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LLMProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1.LLMProvider{}).
		Complete(r)
}
