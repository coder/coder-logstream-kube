package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/codersdk/agentsdk"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/assert"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodEventLogger(t *testing.T) {
	t.Parallel()

	queued := make(chan agentsdk.PatchStartupLogs, 1)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentsdk.PatchStartupLogs
		err := json.NewDecoder(r.Body).Decode(&req)
		assert.NoError(t, err)
		queued <- req
	}))
	agentURL, err := url.Parse(agent.URL)
	require.NoError(t, err)
	namespace := "test-namespace"
	client := fake.NewSimpleClientset()
	ctx := context.Background()
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:    client,
		coderURL:  agentURL,
		namespace: namespace,
		logger:    slogtest.Make(t, nil),
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

	log := <-queued
	require.Len(t, log.Logs, 1)
	require.Contains(t, log.Logs[0].Output, "Created pod")

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

	log = <-queued
	require.Len(t, log.Logs, 1)
	require.Contains(t, log.Logs[0].Output, event.Message)

	err = client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, v1.DeleteOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		reporter.mutex.Lock()
		defer reporter.mutex.Unlock()
		return len(reporter.podToAgentTokens) == 0
	}, time.Second, time.Millisecond)

	require.Eventually(t, func() bool {
		reporter.mutex.Lock()
		defer reporter.mutex.Unlock()
		return len(reporter.agentTokenToLogger) == 0
	}, time.Second, time.Millisecond)

	err = reporter.Close()
	require.NoError(t, err)
}
