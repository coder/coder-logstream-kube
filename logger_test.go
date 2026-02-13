package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

func TestReplicaSetEvents(t *testing.T) {
	t.Parallel()

	api := newFakeAgentAPI(t)

	ctx := testutil.Context(t, testutil.WaitShort)
	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)
	namespace := "test-namespace"
	client := fake.NewSimpleClientset()

	cMock := quartz.NewMock(t)
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
		clock:       cMock,
	})
	require.NoError(t, err)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-rs",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Env: []corev1.EnvVar{
							{
								Name:  "CODER_AGENT_TOKEN",
								Value: "test-token",
							},
						},
					}},
				},
			},
		},
	}
	_, err = client.AppsV1().ReplicaSets(namespace).Create(ctx, rs, v1.CreateOptions{})
	require.NoError(t, err)

	source := testutil.RequireReceive(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source.ID)
	require.Equal(t, "Kubernetes", source.DisplayName)
	require.Equal(t, "/icon/k8s.png", source.Icon)

	logs := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Queued pod from ReplicaSet")

	event := &corev1.Event{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-event",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "ReplicaSet",
			Name: "test-rs",
		},
		Reason:  "Test",
		Message: "Test event",
	}
	_, err = client.CoreV1().Events(namespace).Create(ctx, event, v1.CreateOptions{})
	require.NoError(t, err)

	logs = testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, event.Message)

	err = client.AppsV1().ReplicaSets(namespace).Delete(ctx, rs.Name, v1.DeleteOptions{})
	require.NoError(t, err)

	logs = testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Deleted ReplicaSet")

	require.Eventually(t, func() bool {
		return reporter.tc.isEmpty()
	}, time.Second, time.Millisecond)

	_ = testutil.RequireReceive(ctx, t, api.disconnect)

	err = reporter.Close()
	require.NoError(t, err)
}

func TestPodEvents(t *testing.T) {
	t.Parallel()

	api := newFakeAgentAPI(t)

	ctx := testutil.Context(t, testutil.WaitShort)
	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)
	namespace := "test-namespace"
	client := fake.NewSimpleClientset()

	cMock := quartz.NewMock(t)
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
		clock:       cMock,
	})
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-pod",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token",
						},
					},
				},
			},
		},
	}
	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, v1.CreateOptions{})
	require.NoError(t, err)

	source := testutil.RequireReceive(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source.ID)
	require.Equal(t, "Kubernetes", source.DisplayName)
	require.Equal(t, "/icon/k8s.png", source.Icon)

	logs := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Created pod")

	event := &corev1.Event{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-event",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod",
			Name: "test-pod",
		},
		Reason:  "Test",
		Message: "Test event",
	}
	_, err = client.CoreV1().Events(namespace).Create(ctx, event, v1.CreateOptions{})
	require.NoError(t, err)

	logs = testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, event.Message)

	err = client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, v1.DeleteOptions{})
	require.NoError(t, err)

	logs = testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Deleted pod")

	require.Eventually(t, func() bool {
		return reporter.tc.isEmpty()
	}, time.Second, time.Millisecond)

	_ = testutil.RequireReceive(ctx, t, api.disconnect)

	err = reporter.Close()
	require.NoError(t, err)
}

func TestPodEventsWithSecretRef(t *testing.T) {
	t.Parallel()

	api := newFakeAgentAPI(t)

	ctx := testutil.Context(t, testutil.WaitShort)
	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)
	namespace := "test-namespace"

	// Create the secret first
	secret := &corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      "agent-token-secret",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"token": []byte("secret-token-value"),
		},
	}
	client := fake.NewSimpleClientset(secret)

	cMock := quartz.NewMock(t)
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
		clock:       cMock,
	})
	require.NoError(t, err)

	// Create pod with secretKeyRef for CODER_AGENT_TOKEN
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-pod-secret",
			Namespace: namespace,
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
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
		},
	}
	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, v1.CreateOptions{})
	require.NoError(t, err)

	source := testutil.RequireReceive(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source.ID)
	require.Equal(t, "Kubernetes", source.DisplayName)
	require.Equal(t, "/icon/k8s.png", source.Icon)

	logs := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Created pod")

	err = reporter.Close()
	require.NoError(t, err)
}

func TestReplicaSetEventsWithSecretRef(t *testing.T) {
	t.Parallel()

	api := newFakeAgentAPI(t)

	ctx := testutil.Context(t, testutil.WaitShort)
	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)
	namespace := "test-namespace"

	// Create the secret first
	secret := &corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      "agent-token-secret",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"token": []byte("secret-token-value"),
		},
	}
	client := fake.NewSimpleClientset(secret)

	cMock := quartz.NewMock(t)
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
		clock:       cMock,
	})
	require.NoError(t, err)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-rs-secret",
			Namespace: namespace,
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
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
					}},
				},
			},
		},
	}
	_, err = client.AppsV1().ReplicaSets(namespace).Create(ctx, rs, v1.CreateOptions{})
	require.NoError(t, err)

	source := testutil.RequireReceive(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source.ID)
	require.Equal(t, "Kubernetes", source.DisplayName)
	require.Equal(t, "/icon/k8s.png", source.Icon)

	logs := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Queued pod from ReplicaSet")

	err = reporter.Close()
	require.NoError(t, err)
}

func TestPodEventsWithOptionalMissingSecret(t *testing.T) {
	t.Parallel()

	ctx := testutil.Context(t, testutil.WaitShort)
	namespace := "test-namespace"

	// No secret created - but it's marked as optional
	client := fake.NewSimpleClientset()

	cMock := quartz.NewMock(t)
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    &url.URL{Scheme: "http", Host: "localhost"},
		namespaces:  []string{namespace},
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
		clock:       cMock,
	})
	require.NoError(t, err)

	optional := true
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-pod-optional",
			Namespace: namespace,
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{
							Name: "CODER_AGENT_TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "missing-secret",
									},
									Key:      "token",
									Optional: &optional,
								},
							},
						},
					},
				},
			},
		},
	}
	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, v1.CreateOptions{})
	require.NoError(t, err)

	// Should not register the pod since the optional secret is missing
	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)
	require.True(t, reporter.tc.isEmpty(), "pod should not be registered when optional secret is missing")

	err = reporter.Close()
	require.NoError(t, err)
}

func Test_newPodEventLogger_multipleNamespaces(t *testing.T) {
	t.Parallel()

	api := newFakeAgentAPI(t)

	ctx := testutil.Context(t, testutil.WaitShort)
	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)
	namespaces := []string{"test-namespace1", "test-namespace2"}
	client := fake.NewSimpleClientset()

	cMock := quartz.NewMock(t)
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespaces:  namespaces,
		logger:      slogtest.Make(t, nil).Leveled(slog.LevelDebug),
		logDebounce: 5 * time.Second,
		clock:       cMock,
	})
	require.NoError(t, err)

	// Wait for informer caches to sync before creating pods
	require.True(t, reporter.WaitForCacheSync(ctx), "informer caches failed to sync")

	// Create a pod in the test-namespace1 namespace
	pod1 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-pod-1",
			Namespace: "test-namespace1",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-1",
						},
					},
				},
			},
		},
	}
	_, err = client.CoreV1().Pods("test-namespace1").Create(ctx, pod1, v1.CreateOptions{})
	require.NoError(t, err)

	// Create a pod in the test-namespace2 namespace
	pod2 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-pod-2",
			Namespace: "test-namespace2",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Env: []corev1.EnvVar{
						{
							Name:  "CODER_AGENT_TOKEN",
							Value: "test-token-2",
						},
					},
				},
			},
		},
	}
	_, err = client.CoreV1().Pods("test-namespace2").Create(ctx, pod2, v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for both pods to be registered
	source1 := testutil.RequireReceive(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source1.ID)
	require.Equal(t, "Kubernetes", source1.DisplayName)
	require.Equal(t, "/icon/k8s.png", source1.Icon)

	source2 := testutil.RequireReceive(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source2.ID)
	require.Equal(t, "Kubernetes", source2.DisplayName)
	require.Equal(t, "/icon/k8s.png", source2.Icon)

	// Wait for both creation logs
	logs1 := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs1, 1)
	require.Contains(t, logs1[0].Output, "Created pod")

	logs2 := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, logs2, 1)
	require.Contains(t, logs2[0].Output, "Created pod")

	// Create an event in the first namespace
	event1 := &corev1.Event{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-event-1",
			Namespace: "test-namespace1",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "test-pod-1",
			Namespace: "test-namespace1",
		},
		Reason:  "Test",
		Message: "Test event for namespace1",
	}
	_, err = client.CoreV1().Events("test-namespace1").Create(ctx, event1, v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for the event log
	eventLogs := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, eventLogs, 1)
	require.Contains(t, eventLogs[0].Output, "Test event for namespace1")

	// Create an event in the first namespace
	event2 := &corev1.Event{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-event-2",
			Namespace: "test-namespace2",
			CreationTimestamp: v1.Time{
				Time: time.Now().Add(time.Hour),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "test-pod-2",
			Namespace: "test-namespace2",
		},
		Reason:  "Test",
		Message: "Test event for namespace2",
	}
	_, err = client.CoreV1().Events("test-namespace2").Create(ctx, event2, v1.CreateOptions{})
	require.NoError(t, err)

	// Wait for the event log
	eventLogs2 := testutil.RequireReceive(ctx, t, api.logs)
	require.Len(t, eventLogs2, 1)
	require.Contains(t, eventLogs2[0].Output, "Test event for namespace2")

	// Clean up
	err = reporter.Close()
	require.NoError(t, err)
}

func Test_tokenCache(t *testing.T) {
	t.Parallel()

	t.Run("Pod", func(t *testing.T) {
		t.Parallel()

		tc := tokenCache{
			pods:        map[string][]string{},
			replicaSets: map[string][]string{},
		}

		name := "poddypod"
		tokens := tc.getPodTokens(name)
		require.Len(t, tokens, 0)

		expected := []string{"tokkytok"}
		tokens = tc.setPodToken(name, expected[0])
		require.Equal(t, expected, tokens)

		expected = append(expected, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
		tokens = tc.setPodToken(name, expected[1])
		require.Equal(t, expected, tokens)

		tokens = tc.deletePodToken(name)
		require.Equal(t, expected, tokens)

		tokens = tc.getPodTokens(name)
		require.Len(t, tokens, 0)
	})

	t.Run("ReplicaSet", func(t *testing.T) {
		t.Parallel()

		tc := tokenCache{
			pods:        map[string][]string{},
			replicaSets: map[string][]string{},
		}

		name := "wopwop"
		tokens := tc.getReplicaSetTokens(name)
		require.Len(t, tokens, 0)

		expected := []string{"tokkytok"}
		tokens = tc.setReplicaSetToken(name, expected[0])
		require.Equal(t, expected, tokens)

		expected = append(expected, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
		tokens = tc.setReplicaSetToken(name, expected[1])
		require.Equal(t, expected, tokens)

		tokens = tc.deleteReplicaSetToken(name)
		require.Equal(t, expected, tokens)

		tokens = tc.getReplicaSetTokens(name)
		require.Len(t, tokens, 0)
	})
}

func Test_logQueuer(t *testing.T) {
	t.Run("Timeout", func(t *testing.T) {
		api := newFakeAgentAPI(t)
		agentURL, err := url.Parse(api.server.URL)
		require.NoError(t, err)
		clock := quartz.NewMock(t)
		ttl := time.Second

		ch := make(chan agentLog)
		lq := &logQueuer{
			logger:    slogtest.Make(t, nil),
			clock:     clock,
			q:         ch,
			coderURL:  agentURL,
			loggerTTL: ttl,
			loggers:   map[string]agentLoggerLifecycle{},
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		go lq.work(ctx)

		ch <- agentLog{
			op:           opLog,
			resourceName: "mypod",
			agentToken:   "0b42fa72-7f1a-4b59-800d-69d67f56ed8b",
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "This is a log.",
				Level:     codersdk.LogLevelInfo,
			},
		}

		// it should send both a log source request and the log
		_ = testutil.RequireReceive(ctx, t, api.logSource)
		logs := testutil.RequireReceive(ctx, t, api.logs)
		require.Len(t, logs, 1)

		ch <- agentLog{
			op:           opLog,
			resourceName: "mypod",
			agentToken:   "0b42fa72-7f1a-4b59-800d-69d67f56ed8b",
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "This is a log too.",
				Level:     codersdk.LogLevelInfo,
			},
		}

		// duplicate logs should not trigger a log source
		logs = testutil.RequireReceive(ctx, t, api.logs)
		require.Len(t, logs, 1)

		clock.Advance(ttl)
		// wait for the client to disconnect
		_ = testutil.RequireReceive(ctx, t, api.disconnect)
	})

	t.Run("RetryMechanism", func(t *testing.T) {
		t.Parallel()

		// Create a failing API that will reject connections
		failingAPI := newFailingAgentAPI(t)
		agentURL, err := url.Parse(failingAPI.server.URL)
		require.NoError(t, err)
		clock := quartz.NewMock(t)
		ttl := time.Second

		ch := make(chan agentLog, 10)
		logger := slogtest.Make(t, &slogtest.Options{
			IgnoreErrors: true,
		})
		lq := &logQueuer{
			logger:    logger,
			clock:     clock,
			q:         ch,
			coderURL:  agentURL,
			loggerTTL: ttl,
			loggers:   map[string]agentLoggerLifecycle{},
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
			maxRetries: 10,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		go lq.work(ctx)

		token := "retry-token"
		ch <- agentLog{
			op:           opLog,
			resourceName: "hello",
			agentToken:   token,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "This is a log.",
				Level:     codersdk.LogLevelInfo,
			},
		}

		// Wait for the initial failure to be processed and retry state to be created
		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			rs := lq.retries[token]
			return rs != nil && rs.timer != nil && rs.delay == 2*time.Second
		}, testutil.WaitShort, testutil.IntervalFast)

		// Verify retry state exists and has correct doubled delay (it gets doubled after scheduling)
		lq.mu.Lock()
		rs := lq.retries[token]
		require.NotNil(t, rs)
		require.Equal(t, 2*time.Second, rs.delay) // Delay gets doubled after scheduling
		require.NotNil(t, rs.timer)
		lq.mu.Unlock()

		// Advance clock to trigger first retry
		clock.Advance(time.Second)

		// Wait for retry to be processed and delay to double again
		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			rs := lq.retries[token]
			return rs != nil && rs.delay == 4*time.Second
		}, testutil.WaitShort, testutil.IntervalFast)

		// Check that delay doubled again for next retry
		lq.mu.Lock()
		rs = lq.retries[token]
		require.NotNil(t, rs)
		require.Equal(t, 4*time.Second, rs.delay)
		lq.mu.Unlock()

		// Advance clock to trigger second retry
		clock.Advance(2 * time.Second)

		// Wait for retry to be processed and delay to double again
		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			rs := lq.retries[token]
			return rs != nil && rs.delay == 8*time.Second
		}, testutil.WaitShort, testutil.IntervalFast)

		// Check that delay doubled again
		lq.mu.Lock()
		rs = lq.retries[token]
		require.NotNil(t, rs)
		require.Equal(t, 8*time.Second, rs.delay)
		lq.mu.Unlock()
	})

	t.Run("RetryMaxDelay", func(t *testing.T) {
		t.Parallel()

		clock := quartz.NewMock(t)
		ch := make(chan agentLog, 10)
		lq := &logQueuer{
			logger: slogtest.Make(t, nil),
			clock:  clock,
			q:      ch,
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
			maxRetries: 10,
		}

		ctx := context.Background()
		token := "test-token"

		// Set up a retry state with a large delay
		lq.retries = make(map[string]*retryState)
		lq.retries[token] = &retryState{
			delay:      20 * time.Second,
			retryCount: 0,
		}

		// Schedule a retry - should cap at 30 seconds
		lq.scheduleRetry(ctx, token)

		rs := lq.retries[token]
		require.NotNil(t, rs)
		require.Equal(t, 30*time.Second, rs.delay)

		// Schedule another retry - should stay at 30 seconds
		lq.scheduleRetry(ctx, token)
		rs = lq.retries[token]
		require.NotNil(t, rs)
		require.Equal(t, 30*time.Second, rs.delay)
	})

	t.Run("ClearRetry", func(t *testing.T) {
		t.Parallel()

		clock := quartz.NewMock(t)
		ch := make(chan agentLog, 10)
		lq := &logQueuer{
			logger: slogtest.Make(t, nil),
			clock:  clock,
			q:      ch,
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
			maxRetries: 2,
		}

		ctx := context.Background()
		token := "test-token"

		// Schedule a retry
		lq.scheduleRetry(ctx, token)
		require.NotNil(t, lq.retries[token])

		// Clear the retry
		lq.clearRetryLocked(token)
		require.Nil(t, lq.retries[token])
	})

	t.Run("MaxRetries", func(t *testing.T) {
		t.Parallel()

		// Create a failing API that will reject connections
		failingAPI := newFailingAgentAPI(t)
		agentURL, err := url.Parse(failingAPI.server.URL)
		require.NoError(t, err)
		clock := quartz.NewMock(t)
		ttl := time.Second

		ch := make(chan agentLog, 10)
		logger := slogtest.Make(t, &slogtest.Options{
			IgnoreErrors: true,
		})
		lq := &logQueuer{
			logger:    logger,
			clock:     clock,
			q:         ch,
			coderURL:  agentURL,
			loggerTTL: ttl,
			loggers:   map[string]agentLoggerLifecycle{},
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
			retries:    make(map[string]*retryState),
			maxRetries: 2,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		go lq.work(ctx)

		token := "max-retry-token"
		ch <- agentLog{
			op:           opLog,
			resourceName: "hello",
			agentToken:   token,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "This is a log.",
				Level:     codersdk.LogLevelInfo,
			},
		}

		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			rs := lq.retries[token]
			return rs != nil && rs.retryCount == 1
		}, testutil.WaitShort, testutil.IntervalFast)

		clock.Advance(time.Second)

		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			rs := lq.retries[token]
			return rs != nil && rs.retryCount == 2
		}, testutil.WaitShort, testutil.IntervalFast)

		clock.Advance(2 * time.Second)

		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			rs := lq.retries[token]
			return rs == nil || rs.exhausted
		}, testutil.WaitShort, testutil.IntervalFast)

		lq.mu.Lock()
		cachedLogs := lq.logCache.get(token)
		lq.mu.Unlock()
		require.Nil(t, cachedLogs)
	})
}

func Test_logCache(t *testing.T) {
	t.Parallel()

	t.Run("PushAndGet", func(t *testing.T) {
		t.Parallel()

		lc := logCache{
			logs: map[string][]agentsdk.Log{},
		}

		token := "test-token"

		// Initially should return nil
		logs := lc.get(token)
		require.Nil(t, logs)

		// Push first log
		log1 := agentLog{
			agentToken: token,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "First log",
				Level:     codersdk.LogLevelInfo,
			},
		}
		returnedLogs := lc.push(log1)
		require.Len(t, returnedLogs, 1)
		require.Equal(t, "First log", returnedLogs[0].Output)

		// Get should return the cached logs
		cachedLogs := lc.get(token)
		require.Len(t, cachedLogs, 1)
		require.Equal(t, "First log", cachedLogs[0].Output)

		// Push second log to same token
		log2 := agentLog{
			agentToken: token,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "Second log",
				Level:     codersdk.LogLevelWarn,
			},
		}
		returnedLogs = lc.push(log2)
		require.Len(t, returnedLogs, 2)
		require.Equal(t, "First log", returnedLogs[0].Output)
		require.Equal(t, "Second log", returnedLogs[1].Output)

		// Get should return both logs
		cachedLogs = lc.get(token)
		require.Len(t, cachedLogs, 2)
		require.Equal(t, "First log", cachedLogs[0].Output)
		require.Equal(t, "Second log", cachedLogs[1].Output)
	})

	t.Run("Delete", func(t *testing.T) {
		t.Parallel()

		lc := logCache{
			logs: map[string][]agentsdk.Log{},
		}

		token := "test-token"

		// Push a log
		log := agentLog{
			agentToken: token,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "Test log",
				Level:     codersdk.LogLevelInfo,
			},
		}
		lc.push(log)

		// Verify it exists
		cachedLogs := lc.get(token)
		require.Len(t, cachedLogs, 1)

		// Delete it
		lc.delete(token)

		// Should return nil now
		cachedLogs = lc.get(token)
		require.Nil(t, cachedLogs)
	})

	t.Run("MultipleTokens", func(t *testing.T) {
		t.Parallel()

		lc := logCache{
			logs: map[string][]agentsdk.Log{},
		}

		token1 := "token1"
		token2 := "token2"

		// Push logs for different tokens
		log1 := agentLog{
			agentToken: token1,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "Log for token1",
				Level:     codersdk.LogLevelInfo,
			},
		}
		log2 := agentLog{
			agentToken: token2,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "Log for token2",
				Level:     codersdk.LogLevelError,
			},
		}

		lc.push(log1)
		lc.push(log2)

		// Each token should have its own logs
		logs1 := lc.get(token1)
		require.Len(t, logs1, 1)
		require.Equal(t, "Log for token1", logs1[0].Output)

		logs2 := lc.get(token2)
		require.Len(t, logs2, 1)
		require.Equal(t, "Log for token2", logs2[0].Output)

		// Delete one token shouldn't affect the other
		lc.delete(token1)
		require.Nil(t, lc.get(token1))

		logs2 = lc.get(token2)
		require.Len(t, logs2, 1)
		require.Equal(t, "Log for token2", logs2[0].Output)
	})

	t.Run("EmptyLogHandling", func(t *testing.T) {
		t.Parallel()

		api := newFakeAgentAPI(t)
		agentURL, err := url.Parse(api.server.URL)
		require.NoError(t, err)
		clock := quartz.NewMock(t)
		ttl := time.Second

		ch := make(chan agentLog, 10)
		lq := &logQueuer{
			logger:    slogtest.Make(t, nil),
			clock:     clock,
			q:         ch,
			coderURL:  agentURL,
			loggerTTL: ttl,
			loggers:   map[string]agentLoggerLifecycle{},
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		go lq.work(ctx)

		token := "test-token"

		// Send an empty log first - should be ignored since no cached logs exist
		emptyLog := agentLog{
			op:           opLog,
			resourceName: "",
			agentToken:   token,
			log: agentsdk.Log{
				Output:    "",
				CreatedAt: time.Time{},
			},
		}
		ch <- emptyLog

		// Wait to ensure processing completes - no logger should be created for empty log with no cache
		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			_, exists := lq.loggers[token]
			return !exists
		}, testutil.WaitShort, testutil.IntervalFast)

		// No logger should be created for empty log with no cache
		lq.mu.Lock()
		_, exists := lq.loggers[token]
		require.False(t, exists)
		lq.mu.Unlock()

		// Now send a real log to establish the logger
		realLog := agentLog{
			op:           opLog,
			resourceName: "hello",
			agentToken:   token,
			log: agentsdk.Log{
				CreatedAt: time.Now(),
				Output:    "Real log",
				Level:     codersdk.LogLevelInfo,
			},
		}
		ch <- realLog

		// Should create logger and send log
		_ = testutil.RequireReceive(ctx, t, api.logSource)
		logs := testutil.RequireReceive(ctx, t, api.logs)
		require.Len(t, logs, 1)
		require.Contains(t, logs[0].Output, "Real log")

		// Now send empty log - should trigger flush of any cached logs
		ch <- emptyLog

		// Wait for processing - logger should still exist after empty log
		require.Eventually(t, func() bool {
			lq.mu.Lock()
			defer lq.mu.Unlock()
			_, exists := lq.loggers[token]
			return exists
		}, testutil.WaitShort, testutil.IntervalFast)

		// Logger should still exist
		lq.mu.Lock()
		_, exists = lq.loggers[token]
		require.True(t, exists)
		lq.mu.Unlock()
	})
}

func newFakeAgentAPI(t *testing.T) *fakeAgentAPI {
	logger := slogtest.Make(t, nil)
	mux := drpcmux.New()
	fakeAPI := &fakeAgentAPI{
		disconnect: make(chan struct{}),
		logs:       make(chan []*proto.Log),
		logSource:  make(chan agentsdk.PostLogSourceRequest),
	}
	err := proto.DRPCRegisterAgent(mux, fakeAPI)
	require.NoError(t, err)

	dserver := drpcserver.New(mux)
	fakeAPI.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rtr := chi.NewRouter()

		rtr.Post("/api/v2/workspaceagents/me/log-source", func(w http.ResponseWriter, r *http.Request) {
			fakeAPI.PostLogSource(w, r)
		})

		rtr.Get("/api/v2/workspaceagents/me/rpc", func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				fakeAPI.disconnect <- struct{}{}
			}()

			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				logger.Warn(ctx, "failed to accept websocket", slog.Error(err))
				httpapi.Write(ctx, w, http.StatusBadRequest, codersdk.Response{
					Message: "Failed to accept websocket.",
					Detail:  err.Error(),
				})
				return
			}

			ctx, wsNetConn := codersdk.WebsocketNetConn(ctx, conn, websocket.MessageBinary)
			defer func() {
				_ = wsNetConn.Close()
			}()

			config := yamux.DefaultConfig()
			config.LogOutput = io.Discard
			session, err := yamux.Server(wsNetConn, config)
			if err != nil {
				logger.Warn(ctx, "failed to create yamux", slog.Error(err))
				httpapi.Write(ctx, w, http.StatusBadRequest, codersdk.Response{
					Message: "Failed to accept websocket.",
					Detail:  err.Error(),
				})
				return
			}
			// Ensure session is closed when context is done to unblock Serve
			go func() {
				<-ctx.Done()
				_ = session.Close()
			}()

			err = dserver.Serve(ctx, session)
			logger.Info(ctx, "drpc serveone", slog.Error(err))
		})

		rtr.ServeHTTP(w, r)
	}))
	t.Cleanup(fakeAPI.server.Close)

	return fakeAPI
}

func newFailingAgentAPI(t *testing.T) *fakeAgentAPI {
	fakeAPI := &fakeAgentAPI{
		disconnect: make(chan struct{}),
		logs:       make(chan []*proto.Log),
		logSource:  make(chan agentsdk.PostLogSourceRequest),
	}

	// Create a server that always returns 401 Unauthorized errors
	fakeAPI.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(fakeAPI.server.Close)

	return fakeAPI
}

type fakeAgentAPI struct {
	disconnect chan struct{}
	logs       chan []*proto.Log
	logSource  chan agentsdk.PostLogSourceRequest
	server     *httptest.Server
}

func (*fakeAgentAPI) GetManifest(_ context.Context, _ *proto.GetManifestRequest) (*proto.Manifest, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) GetServiceBanner(_ context.Context, _ *proto.GetServiceBannerRequest) (*proto.ServiceBanner, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) UpdateStats(_ context.Context, _ *proto.UpdateStatsRequest) (*proto.UpdateStatsResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) UpdateLifecycle(_ context.Context, _ *proto.UpdateLifecycleRequest) (*proto.Lifecycle, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) BatchUpdateAppHealths(_ context.Context, _ *proto.BatchUpdateAppHealthRequest) (*proto.BatchUpdateAppHealthResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) UpdateStartup(_ context.Context, _ *proto.UpdateStartupRequest) (*proto.Startup, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) BatchUpdateMetadata(_ context.Context, _ *proto.BatchUpdateMetadataRequest) (*proto.BatchUpdateMetadataResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) GetAnnouncementBanners(_ context.Context, _ *proto.GetAnnouncementBannersRequest) (*proto.GetAnnouncementBannersResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) ScriptCompleted(_ context.Context, _ *proto.WorkspaceAgentScriptCompletedRequest) (*proto.WorkspaceAgentScriptCompletedResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) GetResourcesMonitoringConfiguration(_ context.Context, _ *proto.GetResourcesMonitoringConfigurationRequest) (*proto.GetResourcesMonitoringConfigurationResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) PushResourcesMonitoringUsage(_ context.Context, _ *proto.PushResourcesMonitoringUsageRequest) (*proto.PushResourcesMonitoringUsageResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) CreateSubAgent(_ context.Context, _ *proto.CreateSubAgentRequest) (*proto.CreateSubAgentResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) DeleteSubAgent(_ context.Context, _ *proto.DeleteSubAgentRequest) (*proto.DeleteSubAgentResponse, error) {
	panic("not implemented")
}

func (*fakeAgentAPI) ListSubAgents(_ context.Context, _ *proto.ListSubAgentsRequest) (*proto.ListSubAgentsResponse, error) {
	panic("not implemented")
}
func (*fakeAgentAPI) ReportConnection(_ context.Context, _ *proto.ReportConnectionRequest) (*emptypb.Empty, error) {
	panic("not implemented")
}

func (f *fakeAgentAPI) BatchCreateLogs(_ context.Context, req *proto.BatchCreateLogsRequest) (*proto.BatchCreateLogsResponse, error) {
	f.logs <- req.Logs
	return &proto.BatchCreateLogsResponse{}, nil
}

func (f *fakeAgentAPI) ReportBoundaryLogs(_ context.Context, _ *proto.ReportBoundaryLogsRequest) (*proto.ReportBoundaryLogsResponse, error) {
	return &proto.ReportBoundaryLogsResponse{}, nil
}

func (f *fakeAgentAPI) PostLogSource(w http.ResponseWriter, r *http.Request) {
	var req agentsdk.PostLogSourceRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		fmt.Println("failed to decode:", err.Error())
		return
	}
	f.logSource <- req
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusCreated)
	err = json.NewEncoder(w).Encode(codersdk.WorkspaceAgentLogSource{})
	if err != nil {
		fmt.Println("failed to encode:", err.Error())
	}
}
