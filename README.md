# coder-logstream-kube

[![discord](https://img.shields.io/discord/747933592273027093?label=discord)](https://discord.gg/coder)
[![release](https://img.shields.io/github/v/tag/coder/coder-logstream-kube)](https://github.com/coder/coder-logstream-kube/releases)
[![godoc](https://pkg.go.dev/badge/github.com/coder/coder-logstream-kube.svg)](https://pkg.go.dev/github.com/coder/coder-logstream-kube)
[![license](https://img.shields.io/github/license/coder/coder-logstream-kube)](./LICENSE)

Stream Kubernetes Pod events to the Coder startup logs.

- Easily determine the reason for a pod provision failure, or why a pod is stuck in a pending state.
- Visibility into when pods are OOMKilled, or when they are evicted.
- Filter by namespace, field selector, and label selector to reduce Kubernetes API load.
- Support for watching multiple namespaces or all namespaces cluster-wide.

![Log Stream](./scripts/demo.png)

## Usage

Apply the Helm chart to start streaming logs into your Coder instance:

```console
helm repo add coder-logstream-kube https://helm.coder.com/logstream-kube
helm install coder-logstream-kube coder-logstream-kube/coder-logstream-kube \
    --namespace coder \
    --set url=<your-coder-url-including-http-or-https>
```

### Multi-Namespace Support

By default, `coder-logstream-kube` watches pods in the namespace where it's deployed. You can configure it to watch multiple namespaces or all namespaces:

#### Watch specific namespaces
```console
helm install coder-logstream-kube coder-logstream-kube/coder-logstream-kube \
    --namespace coder \
    --set url=<your-coder-url> \
    --set namespaces="namespace1,namespace2,namespace3"
```

#### Watch all namespaces
```console
helm install coder-logstream-kube coder-logstream-kube/coder-logstream-kube \
    --namespace coder \
    --set url=<your-coder-url> \
    --set namespaces=""
```

When watching multiple namespaces or all namespaces, the chart automatically creates ClusterRole and ClusterRoleBinding resources instead of namespace-scoped Role and RoleBinding.

### Environment Variable Configuration

You can also configure namespaces using the `CODER_NAMESPACE` environment variable:

- Single namespace: `CODER_NAMESPACE=my-namespace`
- Multiple namespaces: `CODER_NAMESPACE=ns1,ns2,ns3`
- All namespaces: `CODER_NAMESPACE=""` (empty string)

> **Note**
> For additional customization (such as customizing the image, pull secrets, annotations, etc.), you can use the
> [values.yaml](helm/values.yaml) file directly.

Your Coder template should be using a `kubernetes_deployment` resource with `wait_for_rollout` set to `false`.

```hcl
resource "kubernetes_deployment" "hello_world" {
  count = data.coder_workspace.me.start_count
  wait_for_rollout = false
  ...
}
```

This ensures all pod events will be sent during initialization and startup.

## How?

Kubernetes provides an [informers](https://pkg.go.dev/k8s.io/client-go/informers) API that streams pod and event data from the API server.

`coder-logstream-kube` listens for pod creation events with containers that have the `CODER_AGENT_TOKEN` environment variable set. All pod events are streamed as logs to the Coder API using the agent token for authentication.

When configured for multiple namespaces, the application creates separate informers for each specified namespace. When configured to watch all namespaces (empty namespace list), it uses cluster-wide informers.

## Custom Certificates

- [`SSL_CERT_FILE`](https://go.dev/src/crypto/x509/root_unix.go#L19): Specifies the path to an SSL certificate.
- [`SSL_CERT_DIR`](https://go.dev/src/crypto/x509/root_unix.go#L25): Identifies which directory to check for SSL certificate files.

## RBAC Permissions

The required permissions depend on the scope of namespaces being watched:

### Single Namespace (Role/RoleBinding)
When watching a single namespace, the application uses namespace-scoped permissions:
- `pods`: get, watch, list
- `events`: get, watch, list  
- `replicasets`: get, watch, list

### Multiple Namespaces or All Namespaces (ClusterRole/ClusterRoleBinding)
When watching multiple namespaces or all namespaces, the application requires cluster-wide permissions with the same resource access but across all namespaces.

The Helm chart automatically determines which type of RBAC resources to create based on your configuration.
