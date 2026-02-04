<div align="center">
  <h1>coder-logstream-kube</h1>
  <p>Stream Kubernetes Pod events to Coder startup logs</p>

  [Installation](#installation) | [Configuration](#configuration) | [Development](#development)

  [![discord](https://img.shields.io/discord/747933592273027093?label=discord)](https://discord.gg/coder)
  [![release](https://img.shields.io/github/v/tag/coder/coder-logstream-kube)](https://github.com/coder/coder-logstream-kube/pkgs/container/coder-logstream-kube)
  [![godoc](https://pkg.go.dev/badge/github.com/coder/coder-logstream-kube.svg)](https://pkg.go.dev/github.com/coder/coder-logstream-kube)
  [![license](https://img.shields.io/github/license/coder/coder-logstream-kube)](./LICENSE)
</div>

---

- Easily determine the reason for a pod provision failure, or why a pod is stuck pending
- Visibility into when pods are OOMKilled or evicted
- Filter by namespace, field selector, and label selector to reduce Kubernetes API load

![Log Stream](./scripts/demo.png)

## Installation

Deploy via Helm chart:

```console
helm repo add coder-logstream-kube https://helm.coder.com/logstream-kube
helm install coder-logstream-kube coder-logstream-kube/coder-logstream-kube \
    --namespace coder \
    --set url=<your-coder-url>
```

For additional customization (image, pull secrets, annotations, etc.), see the [values.yaml](helm/values.yaml) file.

## Configuration

### Multi-Namespace Support

By default, coder-logstream-kube will watch all namespaces in the cluster. To limit which namespaces are monitored, you can specify them in the [values.yaml](helm/values.yaml) file:

```yaml
# Watch specific namespaces only
namespaces: ["default", "kube-system"]

# Watch all namespaces (default)
namespaces: []
```

When `namespaces` is empty or not specified, the service will monitor all namespaces in the cluster.

> **Note**
> For additional customization (such as customizing the image, pull secrets, annotations, etc.), you can use the [values.yaml](helm/values.yaml) file directly.

### CLI Flags / Environment Variables

| Flag | Env Var | Description |
|------|---------|-------------|
| `--coder-url`, `-u` | `CODER_URL` | URL of the Coder instance (required) |
| `--namespaces`, `-n` | `CODER_NAMESPACES` | Comma-separated list of namespaces to watch |
| `--field-selector`, `-f` | `CODER_FIELD_SELECTOR` | Kubernetes field selector for filtering pods |
| `--label-selector`, `-l` | `CODER_LABEL_SELECTOR` | Kubernetes label selector for filtering pods |
| `--kubeconfig`, `-k` | - | Path to kubeconfig file (default: `~/.kube/config`) |

### Custom Certificates

- [`SSL_CERT_FILE`](https://go.dev/src/crypto/x509/root_unix.go#L19): Path to an SSL certificate file
- [`SSL_CERT_DIR`](https://go.dev/src/crypto/x509/root_unix.go#L25): Directory to check for SSL certificate files

## Template Setup

Your Coder template should use a `kubernetes_deployment` resource with `wait_for_rollout` set to `false`:

```hcl
resource "kubernetes_deployment" "hello_world" {
  count            = data.coder_workspace.me.start_count
  wait_for_rollout = false
  ...
}
```

This ensures all pod events are captured during initialization and startup.

## Development

### Makefile Targets

```console
make help              # Show all available targets
make build             # Build the project
make test              # Run unit tests
make test/integration  # Run integration tests (requires KinD)
make lint              # Run golangci-lint and shellcheck
make fmt               # Format Go and shell code
```

### Integration Tests

Integration tests run against a real Kubernetes cluster using [KinD](https://kind.sigs.k8s.io/).

**Prerequisites:** [Docker](https://docs.docker.com/get-docker/), [KinD](https://kind.sigs.k8s.io/docs/user/quick-start/#installation), [kubectl](https://kubernetes.io/docs/tasks/tools/)

```console
make kind/create       # Create KinD cluster
make test/integration  # Run integration tests
make kind/delete       # Clean up
```

## How It Works

Kubernetes provides an [informers](https://pkg.go.dev/k8s.io/client-go/informers) API that streams pod and event data from the API server.

`coder-logstream-kube` listens for Pod and ReplicaSet events with containers that have the `CODER_AGENT_TOKEN` environment variable set. All events are streamed as logs to the Coder API using the agent token for authentication.

## Support

Feel free to [open an issue](https://github.com/coder/coder-logstream-kube/issues/new) if you have questions, run into bugs, or have a feature request.

[Join our Discord](https://discord.gg/coder) to provide feedback and chat with the community!
