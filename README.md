# LiveLLM Proxy Operator

This Kubernetes operator watches for `LLMProvider` custom resources across all namespaces and automatically registers them with the LiveLLM Proxy.

## Prerequisites

- Go 1.21+
- Kubernetes cluster
- LiveLLM Proxy running in the cluster (or reachable from the operator)

## Installation

1. **Install CRD**:
   ```bash
   kubectl apply -f config/crd/bases/proxy.livellm.ai_llmproviders.yaml
   ```

2. **Run the Operator** (Locally):
   ```bash
   # Install dependencies
   go mod tidy

   # Run operator
   # Set PROXY_URL to point to your LiveLLM Proxy instance
   go run main.go --proxy-url "http://localhost:8000"
   ```

   **Run in Cluster**:
   You will need to build a Docker image and deploy it.
   Ensure the operator Pod has permissions to watch `LLMProviders` and `Secrets` in all namespaces.

## Usage

Create an `LLMProvider` resource and a corresponding Secret for the API key:

```yaml
apiVersion: proxy.livellm.ai/v1
kind: LLMProvider
metadata:
  name: my-openai
  namespace: default
spec:
  provider: openai
  apiKeyRef:
    name: openai-secret
    key: api_key
---
apiVersion: v1
kind: Secret
metadata:
  name: openai-secret
  namespace: default
stringData:
  api_key: sk-proj-...
```

The operator will:
1. Detect the new `LLMProvider`.
2. Read the API key from the referenced Secret.
3. Call the LiveLLM Proxy API (`POST /livellm/providers/config`) to register the provider.
4. Update the status of the `LLMProvider` to `Registered: true`.

If you delete the `LLMProvider`, the operator will automatically deregister it from the proxy.
