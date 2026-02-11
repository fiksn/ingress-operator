# Ingress Operator Helm Chart

This Helm chart deploys the ingress-operator, a Kubernetes operator that translates Ingress resources to Gateway API resources (Gateway and HTTPRoute).

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- Gateway API CRDs installed in your cluster
- A Gateway namespace (default: `nginx-fabric`)

## Installing Gateway API CRDs

Before installing this chart, ensure the Gateway API CRDs are installed:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.0.0/standard-install.yaml
```

## Installing the Chart

First, create the Gateway namespace:

```bash
kubectl create namespace nginx-fabric
```

Install the chart:

```bash
helm install ingress-operator ./helm/ingress-operator
```

Or with custom values:

```bash
helm install ingress-operator ./helm/ingress-operator \
  --set operator.gatewayNamespace=my-gateway-namespace \
  --set operator.enableDeletion=true
```

## Uninstalling the Chart

```bash
helm uninstall ingress-operator
```

## Configuration

The following table lists the configurable parameters of the ingress-operator chart and their default values.

### General Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of operator replicas | `1` |
| `image.repository` | Operator image repository | `fiksn/ingress-operator` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `image.tag` | Image tag (overrides chart appVersion) | `latest` |
| `imagePullSecrets` | Image pull secrets | `[]` |
| `nameOverride` | Override chart name | `""` |
| `fullnameOverride` | Override full chart name | `""` |

### Service Account

| Parameter | Description | Default |
|-----------|-------------|---------|
| `serviceAccount.create` | Create service account | `true` |
| `serviceAccount.annotations` | Service account annotations | `{}` |
| `serviceAccount.name` | Service account name | `""` |

### Security

| Parameter | Description | Default |
|-----------|-------------|---------|
| `podSecurityContext.runAsNonRoot` | Run as non-root user | `true` |
| `podSecurityContext.runAsUser` | User ID to run as | `65532` |
| `podSecurityContext.fsGroup` | Filesystem group | `65532` |
| `securityContext.allowPrivilegeEscalation` | Allow privilege escalation | `false` |
| `securityContext.capabilities.drop` | Dropped capabilities | `["ALL"]` |
| `securityContext.readOnlyRootFilesystem` | Read-only root filesystem | `true` |

### Resources

| Parameter | Description | Default |
|-----------|-------------|---------|
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |
| `resources.requests.cpu` | CPU request | `10m` |
| `resources.requests.memory` | Memory request | `64Mi` |

### Operator Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `operator.gatewayNamespace` | Namespace where Gateway resources are created | `nginx-fabric` |
| `operator.gatewayName` | Name of the Gateway resource | `ingress-gateway` |
| `operator.gatewayClassName` | GatewayClass to use | `nginx` |
| `operator.watchNamespace` | Namespace to watch (empty = all namespaces) | `""` |
| `operator.ingressClassFilter` | Glob pattern to filter ingress classes | `"*"` |
| `operator.oneGatewayPerIngress` | Create separate Gateway per Ingress | `false` |
| `operator.enableDeletion` | Delete resources when Ingress is deleted | `false` |
| `operator.hostnameRewriteFrom` | Comma-separated domain suffixes to match | `""` |
| `operator.hostnameRewriteTo` | Comma-separated replacement domain suffixes | `""` |
| `operator.disableSourceIngress` | Disable source Ingress | `false` |
| `operator.private` | Apply private annotations to all Gateways | `false` |
| `operator.privateIngressClassPattern` | Pattern for private ingress classes | `"*private*"` |
| `operator.leaderElect` | Enable leader election | `false` |
| `operator.metricsBindAddress` | Metrics server bind address | `"0"` (disabled) |
| `operator.metricsSecure` | Serve metrics over HTTPS | `true` |
| `operator.healthProbeBindAddress` | Health probe bind address | `":8081"` |
| `operator.enableHTTP2` | Enable HTTP/2 | `false` |
| `operator.useIngress2Gateway` | Use ingress2gateway library | `false` |

### Service Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `service.type` | Service type | `ClusterIP` |
| `service.metricsPort` | Metrics service port | `8443` |
| `service.healthPort` | Health service port | `8081` |

## Examples

### Watch a Specific Namespace

```bash
helm install ingress-operator ./helm/ingress-operator \
  --set operator.watchNamespace=my-app-namespace
```

### Enable Resource Deletion

```bash
helm install ingress-operator ./helm/ingress-operator \
  --set operator.enableDeletion=true
```

### Filter by Ingress Class

```bash
helm install ingress-operator ./helm/ingress-operator \
  --set operator.ingressClassFilter="nginx*"
```

### Enable Metrics

```bash
helm install ingress-operator ./helm/ingress-operator \
  --set operator.metricsBindAddress=":8080" \
  --set operator.metricsSecure=false
```

### One Gateway Per Ingress

```bash
helm install ingress-operator ./helm/ingress-operator \
  --set operator.oneGatewayPerIngress=true
```

## RBAC

The chart creates the following RBAC resources:
- ServiceAccount
- ClusterRole with permissions for:
  - Ingress resources (get, list, watch, update, patch)
  - Gateway API resources (full CRUD)
  - Services (get, list, watch - for port resolution)
  - Namespaces (get, list, watch - for validation)
  - ConfigMaps and Leases (for leader election)
- ClusterRoleBinding

## Troubleshooting

### Check operator logs

```bash
kubectl logs -l app.kubernetes.io/name=ingress-operator -n <namespace>
```

### Verify Gateway namespace exists

```bash
kubectl get namespace nginx-fabric
```

### Check RBAC permissions

```bash
kubectl auth can-i get ingresses --as=system:serviceaccount:<namespace>:<serviceaccount-name>
```

## License

Apache License 2.0
