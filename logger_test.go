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

	"github.com/benbjohnson/clock"
	"github.com/go-chi/chi/v5"
	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"nhooyr.io/websocket"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/coder/v2/testutil"
)

func TestReplicaSetEvents(t *testing.T) {
	t.Parallel()

	api := newFakeAgentAPI(t)

	ctx := testutil.Context(t, testutil.WaitShort)
	agentURL, err := url.Parse(api.server.URL)
	require.NoError(t, err)
	namespace := "test-namespace"
	client := fake.NewSimpleClientset()

	cMock := clock.NewMock()
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespace:   namespace,
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

	source := testutil.RequireRecvCtx(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source.ID)
	require.Equal(t, "Kubernetes", source.DisplayName)
	require.Equal(t, "/icon/k8s.png", source.Icon)

	logs := testutil.RequireRecvCtx(ctx, t, api.logs)
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

	logs = testutil.RequireRecvCtx(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, event.Message)

	err = client.AppsV1().ReplicaSets(namespace).Delete(ctx, rs.Name, v1.DeleteOptions{})
	require.NoError(t, err)

	logs = testutil.RequireRecvCtx(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Deleted ReplicaSet")

	require.Eventually(t, func() bool {
		return reporter.tc.isEmpty()
	}, time.Second, time.Millisecond)

	_ = testutil.RequireRecvCtx(ctx, t, api.disconnect)

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

	cMock := clock.NewMock()
	reporter, err := newPodEventLogger(ctx, podEventLoggerOptions{
		client:      client,
		coderURL:    agentURL,
		namespace:   namespace,
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

	source := testutil.RequireRecvCtx(ctx, t, api.logSource)
	require.Equal(t, sourceUUID, source.ID)
	require.Equal(t, "Kubernetes", source.DisplayName)
	require.Equal(t, "/icon/k8s.png", source.Icon)

	logs := testutil.RequireRecvCtx(ctx, t, api.logs)
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

	logs = testutil.RequireRecvCtx(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, event.Message)

	err = client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, v1.DeleteOptions{})
	require.NoError(t, err)

	logs = testutil.RequireRecvCtx(ctx, t, api.logs)
	require.Len(t, logs, 1)
	require.Contains(t, logs[0].Output, "Deleted pod")

	require.Eventually(t, func() bool {
		return reporter.tc.isEmpty()
	}, time.Second, time.Millisecond)

	_ = testutil.RequireRecvCtx(ctx, t, api.disconnect)

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
		clock := clock.NewMock()
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

		ctx, cancel := context.WithCancel(context.Background())
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
		_ = testutil.RequireRecvCtx(ctx, t, api.logSource)
		logs := testutil.RequireRecvCtx(ctx, t, api.logs)
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
		logs = testutil.RequireRecvCtx(ctx, t, api.logs)
		require.Len(t, logs, 1)

		clock.Add(2 * ttl)
		// wait for the client to disconnect
		_ = testutil.RequireRecvCtx(ctx, t, api.disconnect)
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
			defer wsNetConn.Close()

			config := yamux.DefaultConfig()
			config.LogOutput = io.Discard
			session, err := yamux.Server(wsNetConn, config)
			if err != nil {
				logger.Warn(ctx, "failed to create yamux", slog.Error(err))
				httpapi.Write(ctx, w, http.StatusBadRequest, codersdk.Response{
					Message: "Failed to accept websocket.",
					Detail:  err.Error(),
				})
			}

			err = dserver.Serve(ctx, session)
			logger.Info(ctx, "drpc serveone", slog.Error(err))
		})

		rtr.ServeHTTP(w, r)
	}))

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

func (f *fakeAgentAPI) BatchCreateLogs(_ context.Context, req *proto.BatchCreateLogsRequest) (*proto.BatchCreateLogsResponse, error) {
	f.logs <- req.Logs
	return &proto.BatchCreateLogsResponse{}, nil
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
