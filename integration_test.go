//go:build integration

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/slogtest"
)

// getKubeClient creates a Kubernetes client from the default kubeconfig.
// It will use KUBECONFIG env var if set, otherwise ~/.kube/config.
// It also verifies the cluster is a KinD cluster to prevent accidentally
// running tests against production clusters.
func getKubeClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		require.NoError(t, err, "failed to get user home dir")
		kubeconfig = home + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err, "failed to build kubeconfig")

	// Safety check: ensure we're connecting to a KinD cluster.
	// KinD clusters run on localhost or have "kind" in the host.
	// This prevents accidentally running destructive tests against production clusters.
	if config.Host != "" && os.Getenv("INTEGRATION_TEST_UNSAFE") != "1" {
		isKind := strings.Contains(config.Host, "127.0.0.1") ||
			strings.Contains(config.Host, "localhost") ||
			strings.Contains(strings.ToLower(config.Host), "kind")
		if !isKind {
			t.Fatalf("Safety check failed: integration tests must run against a KinD cluster. "+
				"Current context points to %q. Set KUBECONFIG to a KinD cluster config or "+
				"set INTEGRATION_TEST_UNSAFE=1 to bypass this check.", config.Host)
		}
	}

	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err, "failed to create kubernetes client")

	return client
}

// createTestNamespace creates a unique namespace for test isolation.
// It registers cleanup to delete the namespace after the test.
func createTestNamespace(t *testing.T, ctx context.Context, client kubernetes.Interface) string {
	t.Helper()

	name := fmt.Sprintf("logstream-test-%d", time.Now().UnixNano())

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	_, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create test namespace")

	t.Cleanup(func() {
		// Use a fresh context for cleanup in case the test context is cancelled
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := client.CoreV1().Namespaces().Delete(cleanupCtx, name, metav1.DeleteOptions{})
		if err != nil {
			t.Logf("warning: failed to delete test namespace %s: %v", name, err)
		}
	})

	return name
}

// waitForLogContaining waits until a log containing the given substring is received.
// It collects all logs seen and returns them along with whether the target was found.
func waitForLogContaining(t *testing.T, ctx context.Context, api *fakeAgentAPI, timeout time.Duration, substring string) (allLogs []string, found bool) {
	t.Helper()

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case logs := <-api.logs:
			for _, log := range logs {
				allLogs = append(allLogs, log.Output)
				if strings.Contains(log.Output, substring) {
					return allLogs, true
				}
			}
		case <-timeoutCtx.Done():
			return allLogs, false
		}
	}
}

// waitForLogSource waits for log source registration with a timeout.
func waitForLogSource(t *testing.T, ctx context.Context, api *fakeAgentAPI, timeout time.Duration) {
	t.Helper()

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-api.logSource:
		return
	case <-timeoutCtx.Done():
		t.Fatal("timeout waiting for log source registration")
	}
}

func TestIntegration_PodEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := getKubeClient(t)
	namespace := createTestNamespace(t, ctx, client)

	// Start fake Coder API server
	api := newFakeAgentAPI(t)
	defer api.server.Close()

	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)

	// Create the pod event logger
	// Note: We don't set clock, so it uses a real clock for integration tests
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second, // Use shorter debounce for faster tests
	})
	require.NoError(t, err)
	defer reporter.Close()

	// Wait a bit for informers to sync
	time.Sleep(1 * time.Second)

	// Create a pod with CODER_AGENT_TOKEN
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test-container",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-integration",
						},
					},
				},
			},
			// Use a non-existent node to keep the pod in Pending state
			// This avoids needing to actually run the container
			NodeSelector: map[string]string{
				"non-existent-label": "non-existent-value",
			},
		},
	}

	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source registration
	waitForLogSource(t, ctx, api, 30*time.Second)

	// Wait for the "Created pod" log (may receive other logs first like scheduling warnings)
	logs, found := waitForLogContaining(t, ctx, api, 30*time.Second, "Created pod")
	require.True(t, found, "expected 'Created pod' log, got: %v", logs)

	// Delete the pod and verify deletion event
	err = client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Wait for the "Deleted pod" log
	logs, found = waitForLogContaining(t, ctx, api, 30*time.Second, "Deleted pod")
	require.True(t, found, "expected 'Deleted pod' log, got: %v", logs)
}

func TestIntegration_ReplicaSetEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := getKubeClient(t)
	namespace := createTestNamespace(t, ctx, client)

	// Start fake Coder API server
	api := newFakeAgentAPI(t)
	defer api.server.Close()

	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)

	// Create the pod event logger
	// Note: We don't set clock, so it uses a real clock for integration tests
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second, // Use shorter debounce for faster tests
	})
	require.NoError(t, err)
	defer reporter.Close()

	// Wait a bit for informers to sync
	time.Sleep(1 * time.Second)

	// Create a ReplicaSet with CODER_AGENT_TOKEN
	replicas := int32(1)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rs",
			Namespace: namespace,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test-rs",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test-rs",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "test-container",
							Image:   "busybox:latest",
							Command: []string{"sleep", "3600"},
							Env: []corev1.EnvVar{
								{
									Name:  "CODER_AGENT_TOKEN",
									Value: "test-token-rs-integration",
								},
							},
						},
					},
					// Use a non-existent node to keep pods in Pending state
					NodeSelector: map[string]string{
						"non-existent-label": "non-existent-value",
					},
				},
			},
		},
	}

	_, err = client.AppsV1().ReplicaSets(namespace).Create(ctx, rs, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source registration
	waitForLogSource(t, ctx, api, 30*time.Second)

	// Wait for the "Queued pod from ReplicaSet" log
	logs, found := waitForLogContaining(t, ctx, api, 30*time.Second, "Queued pod from ReplicaSet")
	require.True(t, found, "expected 'Queued pod from ReplicaSet' log, got: %v", logs)

	// Delete the ReplicaSet
	err = client.AppsV1().ReplicaSets(namespace).Delete(ctx, rs.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Wait for the "Deleted ReplicaSet" log
	logs, found = waitForLogContaining(t, ctx, api, 30*time.Second, "Deleted ReplicaSet")
	require.True(t, found, "expected 'Deleted ReplicaSet' log, got: %v", logs)
}

func TestIntegration_MultiNamespace(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := getKubeClient(t)

	// Create two namespaces
	namespace1 := createTestNamespace(t, ctx, client)
	namespace2 := createTestNamespace(t, ctx, client)

	// Start fake Coder API server
	api := newFakeAgentAPI(t)
	defer api.server.Close()

	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)

	// Create the pod event logger watching both namespaces
	// Note: We don't set clock, so it uses a real clock for integration tests
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace1, namespace2},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second, // Use shorter debounce for faster tests
	})
	require.NoError(t, err)
	defer reporter.Close()

	// Wait for informers to sync
	time.Sleep(1 * time.Second)

	// Create a pod in namespace1
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-ns1",
			Namespace: namespace1,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test-container",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-ns1",
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"non-existent-label": "non-existent-value",
			},
		},
	}

	_, err = client.CoreV1().Pods(namespace1).Create(ctx, pod1, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source and logs from first pod
	waitForLogSource(t, ctx, api, 30*time.Second)
	logs, found := waitForLogContaining(t, ctx, api, 30*time.Second, "Created pod")
	require.True(t, found, "expected 'Created pod' log for first pod, got: %v", logs)

	// Create a pod in namespace2
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-ns2",
			Namespace: namespace2,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test-container",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-ns2",
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"non-existent-label": "non-existent-value",
			},
		},
	}

	_, err = client.CoreV1().Pods(namespace2).Create(ctx, pod2, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source and logs from second pod
	waitForLogSource(t, ctx, api, 30*time.Second)
	logs, found = waitForLogContaining(t, ctx, api, 30*time.Second, "Created pod")
	require.True(t, found, "expected 'Created pod' log for second pod, got: %v", logs)

	// Both namespaces should have received events
	t.Log("Successfully received events from both namespaces")
}

func TestIntegration_LabelSelector(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := getKubeClient(t)
	namespace := createTestNamespace(t, ctx, client)

	// Start fake Coder API server
	api := newFakeAgentAPI(t)
	defer api.server.Close()

	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)

	// Create the pod event logger with a label selector
	// Note: We don't set clock, so it uses a real clock for integration tests
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:        client,
		coderURL:      agentURL,
		namespaces:    []string{namespace},
		labelSelector: "coder-workspace=true",
		logger:        slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce:   5 * time.Second, // Use shorter debounce for faster tests
	})
	require.NoError(t, err)
	defer reporter.Close()

	// Wait for informers to sync
	time.Sleep(1 * time.Second)

	// Create a pod WITHOUT the matching label - should be ignored
	podNoLabel := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-no-label",
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test-container",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-no-label",
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"non-existent-label": "non-existent-value",
			},
		},
	}

	_, err = client.CoreV1().Pods(namespace).Create(ctx, podNoLabel, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait a bit to ensure the pod without label is not picked up
	time.Sleep(2 * time.Second)

	// Create a pod WITH the matching label - should be tracked
	podWithLabel := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-with-label",
			Namespace: namespace,
			Labels: map[string]string{
				"coder-workspace": "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test-container",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-with-label",
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"non-existent-label": "non-existent-value",
			},
		},
	}

	_, err = client.CoreV1().Pods(namespace).Create(ctx, podWithLabel, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source registration - this should only happen for the labeled pod
	waitForLogSource(t, ctx, api, 30*time.Second)

	// Wait for logs - look specifically for "Created pod" with the labeled pod name
	logs, found := waitForLogContaining(t, ctx, api, 30*time.Second, "Created pod")
	require.True(t, found, "expected 'Created pod' log for labeled pod, got: %v", logs)

	// Verify that none of the logs mention the unlabeled pod
	for _, log := range logs {
		require.NotContains(t, log, "test-pod-no-label", "should not receive logs for unlabeled pod")
	}
}

func TestIntegration_PodWithSecretRef(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := getKubeClient(t)
	namespace := createTestNamespace(t, ctx, client)

	// Create a secret containing the agent token
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-token-secret",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"token": []byte("secret-token-integration"),
		},
	}
	_, err := client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	// Start fake Coder API server
	api := newFakeAgentAPI(t)
	defer api.server.Close()

	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)

	// Create the pod event logger
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
	})
	require.NoError(t, err)
	defer reporter.Close()

	// Wait for informers to sync
	time.Sleep(1 * time.Second)

	// Create a pod with CODER_AGENT_TOKEN from secretKeyRef
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-secret",
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test-container",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
					Env: []corev1.EnvVar{
						{
							Name: "CODER_AGENT_TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "agent-token-secret",
									},
									Key: "token",
								},
							},
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"non-existent-label": "non-existent-value",
			},
		},
	}

	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source registration
	waitForLogSource(t, ctx, api, 30*time.Second)

	// Wait for the "Created pod" log
	logs, found := waitForLogContaining(t, ctx, api, 30*time.Second, "Created pod")
	require.True(t, found, "expected 'Created pod' log, got: %v", logs)

	// Delete the pod and verify deletion event
	err = client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Wait for the "Deleted pod" log
	logs, found = waitForLogContaining(t, ctx, api, 30*time.Second, "Deleted pod")
	require.True(t, found, "expected 'Deleted pod' log, got: %v", logs)
}

func TestIntegration_ReplicaSetWithSecretRef(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := getKubeClient(t)
	namespace := createTestNamespace(t, ctx, client)

	// Create a secret containing the agent token
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-token-secret",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"token": []byte("secret-token-rs-integration"),
		},
	}
	_, err := client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	// Start fake Coder API server
	api := newFakeAgentAPI(t)
	defer api.server.Close()

	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)

	// Create the pod event logger
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
	})
	require.NoError(t, err)
	defer reporter.Close()

	// Wait for informers to sync
	time.Sleep(1 * time.Second)

	// Create a ReplicaSet with CODER_AGENT_TOKEN from secretKeyRef
	replicas := int32(1)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rs-secret",
			Namespace: namespace,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "test-rs-secret",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test-rs-secret",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "test-container",
							Image:   "busybox:latest",
							Command: []string{"sleep", "3600"},
							Env: []corev1.EnvVar{
								{
									Name: "CODER_AGENT_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "agent-token-secret",
											},
											Key: "token",
										},
									},
								},
							},
						},
					},
					NodeSelector: map[string]string{
						"non-existent-label": "non-existent-value",
					},
				},
			},
		},
	}

	_, err = client.AppsV1().ReplicaSets(namespace).Create(ctx, rs, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for log source registration
	waitForLogSource(t, ctx, api, 30*time.Second)

	// Wait for the "Queued pod from ReplicaSet" log
	logs, found := waitForLogContaining(t, ctx, api, 30*time.Second, "Queued pod from ReplicaSet")
	require.True(t, found, "expected 'Queued pod from ReplicaSet' log, got: %v", logs)

	// Delete the ReplicaSet
	err = client.AppsV1().ReplicaSets(namespace).Delete(ctx, rs.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Wait for the "Deleted ReplicaSet" log
	logs, found = waitForLogContaining(t, ctx, api, 30*time.Second, "Deleted ReplicaSet")
	require.True(t, found, "expected 'Deleted ReplicaSet' log, got: %v", logs)
}
