# ingress-operator
Ingress operator is a way to transition from `nginx-ingress` to `nginx-fabric` and use `Gateway` and `Httproute` resources instead of `Ingress`.

Check [Ingress2Gateway](https://github.com/kubernetes-sigs/ingress2gateway) out too.

The difference is that this operator will transparently create new resources. Those can of course be retrieved from Kubernetes and put into source control
so you can use this tool like Ingress2Gateway with an extra step.
But the real benefit comes from changing the stuff over which you do not have direct control, for instance a
new `Ingress` resource might be provisioned automatically and you cannot change that simply. At the same
time you cannot run `nginx-ingress` controller anymore as most of the infrastructure is already migrated.

!!! Nginx ingress controller is [deprecated](https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/) and
will not get security updates after March 2026 !!!

Additionally the operator enables a "shared mode" (unless you start it with `--one-gateway-per-ingress`) which means you do not need a new `Gateway` for each `Ingress` but
can aggregate multiple vhosts in one thereby saving cluster resources. This way you just get one (or if you change `nginxproxies` crd also more)
Nginx instance(s) which is consistent with previous `nginx-ingress` behaviour. Of course you get less isolation which depending on your case can also be bad for security.

## Build

```bash
nix-shell -p operator-sdk kubebuilder
CGO_ENABLED=0 go build -o bin/manager ./cmd/main.go
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

## Configuration

The operator accepts the following command-line flags:

```bash
--gateway-namespace string              Namespace where the Gateway resource will be created (default: "nginx-fabric")
--gateway-name string                   Name of the Gateway resource when not using ingressClassName (default: "ingress-gateway")
--watch-namespace string                If specified, only watch Ingresses in this namespace (default: watch all namespaces)
--one-gateway-per-ingress               Create a separate Gateway for each Ingress with the same name (default: false)
--enable-deletion                       Delete HTTPRoute and Gateway when Ingress is deleted (default: false)
--hostname-rewrite-from string          Domain suffix to match for rewriting (e.g., 'domain.cc')
--hostname-rewrite-to string            Replacement domain suffix (e.g., 'foo.domain.cc'). Transforms 'a.b.domain.cc' to 'a.b.foo.domain.cc'
--disable-source-ingress                Disable the source Ingress by removing ingressClassName (default: false)
--gateway-annotation-filters string     Comma-separated list of annotation prefixes to exclude from Gateway (default: "ingress.kubernetes.io,cert-manager.io,nginx.ingress.kubernetes.io")
--httproute-annotation-filters string   Comma-separated list of annotation prefixes to exclude from HTTPRoute (default: "ingress.kubernetes.io,cert-manager.io,nginx.ingress.kubernetes.io")
```

## Operating Modes

### Mode 1: Shared Gateways by IngressClass (Default)

Multiple Ingresses share Gateways based on their `ingressClassName` (or annotation):

```bash
./bin/manager
```

**Behavior:**
- Ingresses with the same `spec.ingressClassName` (or `kubernetes.io/ingress.class` annotation) share a Gateway
- Gateway name is determined by the IngressClass name
- If no IngressClass is specified, uses the `--gateway-name` value
- Example: All Ingresses with `ingressClassName: nginx` → Gateway named `nginx`

### Mode 2: One Gateway Per Ingress

Each Ingress gets its own dedicated Gateway:

```bash
./bin/manager --one-gateway-per-ingress
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
./bin/manager --watch-namespace=test-namespace
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
./bin/manager \
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
./bin/manager --disable-source-ingress
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
./bin/manager \
  --hostname-rewrite-from=domain.cc \
  --hostname-rewrite-to=migration.domain.cc

# Step 2: Test the Gateway setup at migration.domain.cc hostnames

# Step 3: When ready, disable source Ingress and use original hostnames
./bin/manager --disable-source-ingress

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
./bin/manager

# Deletion enabled - resources are cleaned up when Ingress is deleted
./bin/manager --enable-deletion
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
make build-installer IMG=<some-registry>/ingress-operator:tag
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
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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

