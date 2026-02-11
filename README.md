# ingress-operator
Ingress operator is a way to transition from [ingress-nginx](https://github.com/kubernetes/ingress-nginx) to [nginx-gateway-fabric](https://github.com/nginx/nginx-gateway-fabric)
and get started with the [Gateway API](https://gateway-api.sigs.k8s.io/guides/getting-started/)

It can transparently create `Gateway` and `Httproute` resources from `Ingress` (and possibly even delete the `Ingress` altogether afterwards).

!!! Nginx ingress controller is [deprecated](https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/) and
will not get security updates after March 2026 !!!

## Related tools

* [Ingress2Gateway](https://github.com/kubernetes-sigs/ingress2gateway) is a similar tool if you have control over your YAML files and want to update them.
Unfortunately that is not always the case and an `Ingress` might be provisioned without your control. Ingress-operator includes the mentioned tool as a library
so you can benefit from all translation quirks implemented there.

However this tool is primarily meant for `ingress-nginx` audience and comes with a few opinionated but sane defaults.

For instance operator enables a "shared mode" (unless you start it with `--one-gateway-per-ingress`) which means you do not need a new `Gateway` for each `Ingress` but
can aggregate multiple vhosts in one thereby save cluster resources. This way you just get one (or if you change `nginxproxies` crd also more)
Nginx instance(s) which is consistent with previous behaviour. Of course you get less isolation which depending on your case might also be bad for security.

## Build

```bash
nix-shell -p operator-sdk kubebuilder

# Build operator
CGO_ENABLED=0 go build -o bin/operator ./cmd/operator/main.go

# Build webhook
CGO_ENABLED=0 go build -o bin/webhook ./cmd/webhook/main.go
```

Or use the Makefile:
```bash
make build          # Build bin/operator
make build-webhook  # Build bin/webhook
```

## Features

### Cluster-Wide Ingress Watching
The operator monitors all Ingress resources across all namespaces and aggregates them into a unified Gateway configuration.

### Resource Management with Annotations
All synthesized resources are annotated with:
```yaml
annotations:
  ingress-operator.fiction.si/managed-by: ingress-controller
```

**Important:** The operator will NOT overwrite existing Gateway or HTTPRoute resources unless they have this annotation. This prevents conflicts with manually created resources.

### Resource Placement
- **Gateway**: Created in the configured namespace (default: `nginx-fabric`)
- **HTTPRoutes**: Created in the same namespace as their source Ingress

### TLS Configuration
The operator reuses TLS configurations from Ingress resources:
- Hostnames from Ingress rules
- Certificate references from Ingress TLS specs

## Webhook Mode

### Overview

The ingress-operator includes an **admission webhook** that intercepts Ingress creation requests and:
1. Translates the Ingress to Gateway + HTTPRoute resources
2. Creates the Gateway and HTTPRoute in the cluster
3. **Rejects the Ingress** by default (prevents it from being stored)

This mode is useful when you want to completely block Ingress resources from being created while automatically creating the equivalent Gateway API resources.
However it is not best practice since creating other resources is a side-effect - if possible stay with operator approach.

### Why Use Webhook Mode?

- **Prevent Ingress creation entirely**: No Ingress resources in your cluster
- **Automatic translation**: Happens synchronously during admission
- **No operator watching**: Webhook is stateless, no reconciliation loop needed
- **Migration enforcement**: Forces users to use Gateway API by blocking Ingress

### Deploying the Webhook

#### Step 1: Build the Webhook Binary

```bash
CGO_ENABLED=0 go build -o bin/webhook ./cmd/webhook/main.go
```

#### Step 2: Create TLS Certificates

The webhook requires TLS certificates. You can use cert-manager:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ingress-webhook-cert
  namespace: ingress-operator-system
spec:
  secretName: ingress-webhook-tls
  dnsNames:
    - ingress-webhook.ingress-operator-system.svc
    - ingress-webhook.ingress-operator-system.svc.cluster.local
  issuerRef:
    name: selfsigned-issuer
    kind: ClusterIssuer
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
```

#### Step 3: Deploy the Webhook Server

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ingress-webhook
  namespace: ingress-operator-system
spec:
  replicas: 2
  selector:
    matchLabels:
      app: ingress-webhook
  template:
    metadata:
      labels:
        app: ingress-webhook
    spec:
      containers:
      - name: webhook
        image: your-registry/ingress-operator-webhook:latest
        imagePullPolicy: Always
        command:
          - /webhook
        args:
          - --gateway-namespace=nginx-fabric
          - --gateway-name=ingress-gateway
          - --webhook-port=9443
          - --cert-dir=/tmp/k8s-webhook-server/serving-certs
          - --use-ingress2gateway=false
          - --hostname-rewrite-from=
          - --hostname-rewrite-to=
        ports:
        - containerPort: 9443
          name: webhook
          protocol: TCP
        - containerPort: 8080
          name: metrics
          protocol: TCP
        volumeMounts:
        - name: cert
          mountPath: /tmp/k8s-webhook-server/serving-certs
          readOnly: true
        resources:
          limits:
            cpu: 500m
            memory: 256Mi
          requests:
            cpu: 100m
            memory: 128Mi
      volumes:
      - name: cert
        secret:
          secretName: ingress-webhook-tls
---
apiVersion: v1
kind: Service
metadata:
  name: ingress-webhook
  namespace: ingress-operator-system
spec:
  ports:
  - name: webhook
    port: 443
    targetPort: 9443
  - name: metrics
    port: 8080
    targetPort: 8080
  selector:
    app: ingress-webhook
```

#### Step 4: Create the ValidatingWebhookConfiguration

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: ingress-to-gateway-webhook
  annotations:
    cert-manager.io/inject-ca-from: ingress-operator-system/ingress-webhook-cert
webhooks:
  - name: vingress.fiction.si
    admissionReviewVersions: ["v1"]
    clientConfig:
      service:
        name: ingress-webhook
        namespace: ingress-operator-system
        path: /mutate-v1-ingress
      # CA bundle will be injected by cert-manager
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["networking.k8s.io"]
        apiVersions: ["v1"]
        resources: ["ingresses"]
        scope: "*"
    failurePolicy: Fail
    sideEffects: None
    timeoutSeconds: 10
    namespaceSelector:
      matchExpressions:
        # Exclude kube-system and ingress-operator-system namespaces
        - key: kubernetes.io/metadata.name
          operator: NotIn
          values: ["kube-system", "ingress-operator-system"]
```

### Webhook Configuration Flags

```bash
--gateway-namespace string              Namespace where Gateway resources will be created (default: "default")
--gateway-name string                   Name of the Gateway resource (default: "ingress-gateway")
--webhook-port int                      The port the webhook server binds to (default: 9443)
--cert-dir string                       Directory containing TLS certificates (default: "/tmp/k8s-webhook-server/serving-certs")
--metrics-bind-address string           Metrics endpoint address (default: ":8080")
--health-probe-bind-address string      Health probe endpoint address (default: ":8081")
--hostname-rewrite-from string          Domain suffix to match for rewriting
--hostname-rewrite-to string            Replacement domain suffix
--gateway-annotations string            Comma-separated key=value pairs for Gateway annotations
--gateway-annotation-filters string     Comma-separated list of annotation prefixes to exclude from Gateway
--httproute-annotation-filters string   Comma-separated list of annotation prefixes to exclude from HTTPRoute
--use-ingress2gateway                   Use ingress2gateway library for translation (default: false)
--ingress2gateway-provider string       Provider for ingress2gateway (default: "ingress-nginx")
--ingress2gateway-ingress-class string  Ingress class for ingress2gateway filtering (default: "nginx")
```

### Translation Modes

The webhook supports two translation modes:

#### Built-in Translation (default)
```bash
--use-ingress2gateway=false
--hostname-rewrite-from=domain.cc
--hostname-rewrite-to=migration.domain.cc
```

- Uses custom translation logic
- Supports hostname rewriting for migration
- Supports certificate mangling for transformed hostnames
- Full control over Gateway/HTTPRoute structure

#### ingress2gateway Library Mode
```bash
--use-ingress2gateway=true
--ingress2gateway-provider=ingress-nginx
--ingress2gateway-ingress-class=nginx
```

- Uses the official [ingress2gateway](https://github.com/kubernetes-sigs/ingress2gateway) library
- Provider-specific translation (ingress-nginx, istio, kong, etc.)
- **NO hostname rewriting** - uses translation as-is
- **NO certificate mangling** - preserves original TLS configuration
- Annotation copying still works (filters and defaults applied)
- Infrastructure annotations still applied

**When to use ingress2gateway mode:**
- You want standardized, provider-specific translation
- You don't need hostname rewriting
- You want to leverage upstream ingress2gateway logic
- You need provider-specific features (e.g., ingress-nginx canary, istio virtualservice)

### Special Annotations

The webhook recognizes these annotations on Ingress resources:

#### `ingress-operator.fiction.si/ignore-ingress: "true"`
Skip all webhook processing - the Ingress is allowed through unchanged.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: legacy-app
  annotations:
    ingress-operator.fiction.si/ignore-ingress: "true"
spec:
  # This Ingress will be created as-is, no Gateway/HTTPRoute created
```

#### `ingress-operator.fiction.si/allow-ingress: "true"`
Create Gateway/HTTPRoute AND allow the Ingress to be created (for compatibility mode).

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: hybrid-app
  annotations:
    ingress-operator.fiction.si/allow-ingress: "true"
spec:
  # Both Ingress and Gateway+HTTPRoute will be created
```

### Behavior

**Default behavior (no annotations):**
1. Webhook receives Ingress creation request
2. Translates Ingress → Gateway + HTTPRoute
3. Creates Gateway and HTTPRoute resources
4. **Rejects the Ingress** with message:
   ```
   Error from server: admission webhook "vingress.fiction.si" denied the request:
   Ingress resources are not allowed - Gateway and HTTPRoute have been created instead.
   Use annotation 'ingress-operator.fiction.si/allow-ingress=true' to allow Ingress creation.
   ```

**With `allow-ingress=true`:**
1. Creates Gateway and HTTPRoute
2. Allows the Ingress to be created
3. Both resource types exist in the cluster

**With `ignore-ingress=true`:**
1. No translation occurs
2. Ingress is created as-is
3. No Gateway or HTTPRoute created

### Testing the Webhook

```bash
# This will be rejected (Gateway/HTTPRoute created instead)
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: test-ingress
  namespace: default
spec:
  ingressClassName: nginx
  rules:
  - host: test.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: test-service
            port:
              number: 80
EOF

# Check that Gateway and HTTPRoute were created
kubectl get gateway -n nginx-fabric
kubectl get httproute -n default

# This will be allowed (compatibility mode)
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: test-ingress-allowed
  namespace: default
  annotations:
    ingress-operator.fiction.si/allow-ingress: "true"
spec:
  ingressClassName: nginx
  rules:
  - host: test.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: test-service
            port:
              number: 80
EOF
```

### Webhook vs Operator Mode

| Feature | Webhook Mode | Operator Mode |
|---------|-------------|---------------|
| Blocks Ingress creation | ✅ Yes (default) | ❌ No |
| Synchronous | ✅ Yes | ❌ No (async reconciliation) |
| Stateless | ✅ Yes | ❌ No (watches resources) |
| Supports deletion | ❌ No | ✅ Yes (with `--enable-deletion`) |
| Shared Gateways | ❌ No | ✅ Yes |
| Requires certificates | ✅ Yes | ❌ No |
| Failure handling | Blocks on failure | Retries on failure |

### Troubleshooting

**Webhook not being called:**
- Check webhook service is running: `kubectl get pods -n ingress-operator-system`
- Verify service endpoints: `kubectl get endpoints -n ingress-operator-system`
- Check ValidatingWebhookConfiguration exists: `kubectl get validatingwebhookconfiguration`
- Verify CA bundle injected: `kubectl get validatingwebhookconfigurations ingress-to-gateway-webhook -o yaml | grep caBundle`

**Certificate errors:**
- Check cert-manager is installed: `kubectl get pods -n cert-manager`
- Verify certificate is ready: `kubectl get certificate -n ingress-operator-system`
- Check secret created: `kubectl get secret ingress-webhook-tls -n ingress-operator-system`

**Webhook rejecting everything:**
- Check logs: `kubectl logs -n ingress-operator-system -l app=ingress-webhook`
- Verify gateway namespace exists: `kubectl get namespace nginx-fabric`
- Check RBAC permissions for creating Gateway/HTTPRoute

## Operator Configuration

The operator accepts the following command-line flags:

```bash
--gateway-namespace string                    Namespace where the Gateway resource will be created (default: "nginx-fabric")
--gateway-name string                         Name of the Gateway resource when not using ingressClassName (default: "ingress-gateway")
--watch-namespace string                      If specified, only watch Ingresses in this namespace (default: watch all namespaces)
--one-gateway-per-ingress                     Create a separate Gateway for each Ingress with the same name (default: false)
--enable-deletion                             Delete HTTPRoute and Gateway when Ingress is deleted (default: false)
--hostname-rewrite-from string                Domain suffix to match for rewriting (e.g., 'domain.cc')
--hostname-rewrite-to string                  Replacement domain suffix (e.g., 'foo.domain.cc'). Transforms 'a.b.domain.cc' to 'a.b.foo.domain.cc'
--disable-source-ingress                      Disable the source Ingress by removing ingressClassName (default: false)
--gateway-annotation-filters string           Comma-separated list of annotation prefixes to exclude from Gateway (default: "ingress.kubernetes.io,cert-manager.io,nginx.ingress.kubernetes.io")
--httproute-annotation-filters string         Comma-separated list of annotation prefixes to exclude from HTTPRoute (default: "ingress.kubernetes.io,cert-manager.io,nginx.ingress.kubernetes.io")
--use-ingress2gateway                         Use ingress2gateway library for translation (disables hostname/certificate mangling) (default: false)
--ingress2gateway-provider string             Provider to use with ingress2gateway (e.g., ingress-nginx, istio, kong) (default: "ingress-nginx")
--ingress2gateway-ingress-class string        Ingress class name for provider-specific filtering in ingress2gateway (default: "nginx")
--gateway-annotations string                  Comma-separated key=value pairs for Gateway metadata annotations
--gateway-infrastructure-annotations string   Comma-separated key=value pairs for Gateway infrastructure annotations
--private-annotations string                  Comma-separated key=value pairs defining what 'private' means for infrastructure annotations
--private                                     If true, apply private annotations to all Gateways (default: false)
--private-ingress-class-pattern string        Glob pattern for ingress class names that should get private infrastructure annotations (default: "*private*")
```

### Translation Modes in Operator

The operator supports the same two translation modes as the webhook:

**Built-in mode (default):** Full control with hostname rewriting and certificate mangling
**ingress2gateway mode:** Uses upstream library, no hostname/cert mangling, annotation copying still works

Example using ingress2gateway mode:
```bash
./bin/operator \
  --use-ingress2gateway=true \
  --ingress2gateway-provider=ingress-nginx \
  --ingress2gateway-ingress-class=nginx \
  --gateway-namespace=nginx-fabric
```

## Operating Modes

### Mode 1: Shared Gateways by IngressClass (Default)

Multiple Ingresses share Gateways based on their `ingressClassName` (or annotation):

```bash
./bin/operator
```

**Behavior:**
- Ingresses with the same `spec.ingressClassName` (or `kubernetes.io/ingress.class` annotation) share a Gateway
- Gateway name is determined by the IngressClass name
- If no IngressClass is specified, uses the `--gateway-name` value
- Example: All Ingresses with `ingressClassName: nginx` → Gateway named `nginx`

### Mode 2: One Gateway Per Ingress

Each Ingress gets its own dedicated Gateway:

```bash
./bin/operator --one-gateway-per-ingress
```

**Behavior:**
- Each Ingress creates a Gateway with the same name
- IngressClass is ignored
- Gateway and HTTPRoute have the same name as the Ingress
- Gateway is created in `--gateway-namespace`, HTTPRoute in the Ingress namespace

### Namespace Filtering

By default, the operator watches Ingresses in **all namespaces**. You can restrict it to a specific namespace for testing:

```bash
# Watch only the test-namespace
./bin/operator --watch-namespace=test-namespace
```

This is useful for:
- Testing the operator on a subset of Ingresses
- Gradual rollout in production
- Multi-tenant clusters where different operators manage different namespaces

## Migration Strategy

### DNS Conflict Avoidance

When migrating from nginx-ingress to nginx-fabric (Gateway API), you'll face a DNS conflict issue: both the Ingress and Gateway resources will try to claim the same hostnames, and external-dns will be confused.

**Solution: Hostname Rewriting**

Use `--hostname-rewrite-from` and `--hostname-rewrite-to` to transform hostnames in Gateway and HTTPRoute resources:

```bash
./bin/operator \
  --hostname-rewrite-from=domain.cc \
  --hostname-rewrite-to=migration.domain.cc
```

**Example transformation:**
- Original Ingress hostname: `api.service.domain.cc`
- Gateway listener hostname: `api.service.migration.domain.cc`
- HTTPRoute hostname: `api.service.migration.domain.cc`

**Certificate Handling:**

When hostname rewriting is enabled, the operator automatically handles TLS certificates:

1. **Certificate Match Check**: Verifies if the original certificate covers the transformed hostname
2. **Wildcard Support**: Recognizes wildcard certificates (e.g., `*.domain.cc` matches `foo.domain.cc`)
3. **Auto Secret Rename**: If the certificate doesn't match the transformed hostname:
   - Generates a new secret name: `<transformed-hostname>-tls` (e.g., `api-service-migration-domain-cc-tls`)
   - Adds annotation `ingress-operator.fiction.si/certificate-mismatch` with details
   - cert-manager will detect the new secret reference and issue a certificate for the transformed hostname

**Example:**
- Original hostname: `api.domain.cc` with certificate in secret `api-tls`
- Transformed to: `api.migration.domain.cc`
- Certificate in `api-tls` doesn't cover `api.migration.domain.cc`
- Gateway references new secret: `api-migration-domain-cc-tls`
- cert-manager issues new certificate for `api.migration.domain.cc`

This allows you to:
1. Run both nginx-ingress and nginx-fabric side-by-side
2. Test the Gateway setup without DNS conflicts
3. Let cert-manager automatically issue certificates for transformed hostnames
4. Point DNS to the new Gateway when ready

### Disabling Source Ingress

Use `--disable-source-ingress` to automatically disable the original Ingress:

```bash
./bin/operator --disable-source-ingress
```

This will:
- Save the original `spec.ingressClassName` to annotation `ingress-operator.fiction.si/original-ingress-classname`
- Remove `spec.ingressClassName` from the Ingress
- Save the original `kubernetes.io/ingress.class` annotation to `ingress-operator.fiction.si/original-ingress-class`
- Remove `kubernetes.io/ingress.class` annotation
- Add `ingress-operator.fiction.si/disabled: "true"` annotation

This prevents nginx-ingress from processing the Ingress while keeping it in the cluster for reference.

### Complete Migration Example

```bash
# Step 1: Create Gateway/HTTPRoute with transformed hostnames (no DNS conflict)
./bin/operator \
  --hostname-rewrite-from=domain.cc \
  --hostname-rewrite-to=migration.domain.cc

# Step 2: Test the Gateway setup at migration.domain.cc hostnames

# Step 3: When ready, disable source Ingress and use original hostnames
./bin/operator --disable-source-ingress

# Step 4: Update DNS to point to Gateway

# Step 5: Clean up old Ingresses when satisfied
```

## Deletion Behavior

By default (`--enable-deletion=false`), the operator **does NOT delete** Gateway and HTTPRoute resources when an Ingress is deleted. This is the safe default to prevent accidental deletion of resources that might be in use.

When `--enable-deletion=true`:
- **HTTPRoute Deletion**: Always deleted when the corresponding Ingress is deleted
- **Gateway Deletion**:
  - In `--one-gateway-per-ingress` mode: Gateway is deleted (since it's dedicated to that Ingress)
  - In shared mode: Gateway is **NOT** deleted (other Ingresses may be using it)
- **Finalizers**: When deletion is enabled, a finalizer is added to Ingresses to ensure cleanup happens properly

**Example:**
```bash
# Deletion disabled (default) - resources remain after Ingress deletion
./bin/operator

# Deletion enabled - resources are cleaned up when Ingress is deleted
./bin/operator --enable-deletion
```

## Behavior

The operator:
1. **Watches** Ingress resources (all namespaces or filtered)
2. **Prints** synthesized Gateway and HTTPRoute resources to stdout
3. **Creates/Updates** the actual Gateway and HTTPRoute resources in the cluster
4. **Respects** existing resources without the `managed-by` annotation
5. **Copies annotations** from Ingress to Gateway and HTTPRoute (excluding filtered prefixes)
6. **Converts PathType** ImplementationSpecific to PathPrefix with a warning
7. **Groups Ingresses** by IngressClass (in shared mode) or creates individual Gateways (in one-per-ingress mode)
8. **Adds finalizers** when deletion is enabled to ensure proper cleanup

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=fiksn/ingress-operator:v0.0.1
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=fiksn/ingress-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=fiksn/ingress-operator:0.0.1
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/fiksn/ingress-operator/master/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing

Feel free to contribute pull requests.

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright Gregor Pogacnik 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

