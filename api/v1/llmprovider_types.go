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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeySelector selects a key of a Secret.
type SecretKeySelector struct {
	// Name of the referent.
	Name string `json:"name"`
	// The key of the secret to select from.  Must be a valid secret key.
	Key string `json:"key"`
}

// LLMProviderSpec defines the desired state of LLMProvider
type LLMProviderSpec struct {
	// Provider type (e.g., openai, google, anthropic)
	Provider string `json:"provider"`

	// Optional base URL for the provider
	// +optional
	BaseURL string `json:"baseUrl,omitempty"`

	// Models to blacklist
	// +optional
	BlacklistModels []string `json:"blacklistModels,omitempty"`

	// APIKeyRef references a Secret containing the API key
	APIKeyRef SecretKeySelector `json:"apiKeyRef"`
}

// LLMProviderStatus defines the observed state of LLMProvider
type LLMProviderStatus struct {
	// Registered indicates if the provider is successfully registered with the proxy
	Registered bool `json:"registered"`
	// Message contains any error or status message
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// LLMProvider is the Schema for the llmproviders API
type LLMProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMProviderSpec   `json:"spec,omitempty"`
	Status LLMProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LLMProviderList contains a list of LLMProvider
type LLMProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMProvider{}, &LLMProviderList{})
}
