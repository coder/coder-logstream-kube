// Package main implements coder-logstream-kube, a Kubernetes controller
// that streams pod logs to the Coder agent API.
package main

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"cdr.dev/slog/v3"
	"golang.org/x/mod/semver"
	"storj.io/drpc"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/quartz"

	// *Never* remove this. Certificates are not bundled as part
	// of the container, so this is necessary for all connections
	// to not be insecure.
	_ "github.com/breml/rootcerts"
)

type podEventLoggerOptions struct {
	client   kubernetes.Interface
	clock    quartz.Clock
	coderURL *url.URL

	logger      slog.Logger
	logDebounce time.Duration
	// maxRetries is the maximum number of retries for a log send failure.
	maxRetries int

	// The following fields are optional!
	namespaces    []string
	fieldSelector string
	labelSelector string
}

// newPodEventLogger creates a set of Kubernetes informers that listen for
// pods with containers that have the `CODER_AGENT_TOKEN` environment variable.
// Pod events are then streamed as startup logs to that agent via the Coder API.
func newPodEventLogger(ctx context.Context, opts podEventLoggerOptions) (*podEventLogger, error) {
	if opts.logDebounce == 0 {
		opts.logDebounce = 30 * time.Second
	}
	if opts.clock == nil {
		opts.clock = quartz.NewReal()
	}

	if opts.maxRetries == 0 {
		opts.maxRetries = 10
	}

	logCh := make(chan agentLog, 512)
	ctx, cancelFunc := context.WithCancel(ctx)
	reporter := &podEventLogger{
		podEventLoggerOptions: &opts,
		stopChan:              make(chan struct{}),
		errChan:               make(chan error, 16),
		ctx:                   ctx,
		cancelFunc:            cancelFunc,
		logCh:                 logCh,
		tc: &tokenCache{
			pods:        map[string][]string{},
			replicaSets: map[string][]string{},
		},
		lq: &logQueuer{
			logger:    opts.logger,
			clock:     opts.clock,
			q:         logCh,
			coderURL:  opts.coderURL,
			loggerTTL: opts.logDebounce,
			loggers:   map[string]agentLoggerLifecycle{},
			logCache: logCache{
				logs: map[string][]agentsdk.Log{},
			},
			maxRetries: opts.maxRetries,
		},
		doneChan: make(chan struct{}),
	}

	// Start the work goroutine once
	go reporter.lq.work(reporter.ctx, reporter.doneChan)

	// If no namespaces are provided, we listen for events in all namespaces.
	if len(opts.namespaces) == 0 {
		if err := reporter.initNamespace(""); err != nil {
			reporter.cancelFunc()
			<-reporter.doneChan
			return nil, fmt.Errorf("init namespace: %w", err)
		}
	} else {
		for _, namespace := range opts.namespaces {
			if err := reporter.initNamespace(namespace); err != nil {
				reporter.cancelFunc()
				<-reporter.doneChan
				return nil, err
			}
		}
	}

	return reporter, nil
}

type podEventLogger struct {
	*podEventLoggerOptions

	stopChan chan struct{}
	errChan  chan error

	ctx        context.Context
	cancelFunc context.CancelFunc
	tc         *tokenCache

	logCh chan<- agentLog
	lq    *logQueuer

	// hasSyncedFuncs tracks informer cache sync functions for testing
	hasSyncedFuncs []cache.InformerSynced

	// closeOnce ensures Close() is idempotent
	closeOnce sync.Once
	// doneChan is closed when the work goroutine exits
	doneChan chan struct{}
}

// resolveEnvValue resolves the value of an environment variable, supporting both
// direct values and secretKeyRef references. Returns empty string if the value
// cannot be resolved (e.g., optional secret not found).
func (p *podEventLogger) resolveEnvValue(ctx context.Context, namespace string, env corev1.EnvVar) (string, error) {
	// Direct value takes precedence (existing behavior)
	if env.Value != "" {
		return env.Value, nil
	}

	// Check for secretKeyRef
	if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
		ref := env.ValueFrom.SecretKeyRef
		secret, err := p.client.CoreV1().Secrets(namespace).Get(ctx, ref.Name, v1.GetOptions{})
		if err != nil {
			// Handle optional secrets gracefully - only ignore NotFound errors
			if ref.Optional != nil && *ref.Optional && k8serrors.IsNotFound(err) {
				return "", nil
			}
			return "", fmt.Errorf("get secret %s: %w", ref.Name, err)
		}
		value, ok := secret.Data[ref.Key]
		if !ok {
			if ref.Optional != nil && *ref.Optional {
				return "", nil
			}
			return "", fmt.Errorf("secret %s has no key %s", ref.Name, ref.Key)
		}
		return string(value), nil
	}

	return "", nil
}

// initNamespace starts the informer factory and registers event handlers for a given namespace.
// If provided namespace is empty, it will start the informer factory and register event handlers for all namespaces.
func (p *podEventLogger) initNamespace(namespace string) error {
	// We only track events that happen after the reporter starts.
	// This is to prevent us from sending duplicate events.
	startTime := time.Now()

	podFactory := informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithNamespace(namespace), informers.WithTweakListOptions(func(lo *v1.ListOptions) {
		lo.FieldSelector = p.fieldSelector
		lo.LabelSelector = p.labelSelector
	}))
	eventFactory := podFactory
	if p.fieldSelector != "" || p.labelSelector != "" {
		// Events cannot filter on labels and fields!
		eventFactory = informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithNamespace(namespace))
	}

	// We listen for Pods and Events in the informer factory.
	// When a Pod is created, it's added to the map of Pods we're
	// interested in. When a Pod is deleted, it's removed from the map.
	podInformer := podFactory.Core().V1().Pods().Informer()
	replicaInformer := podFactory.Apps().V1().ReplicaSets().Informer()
	eventInformer := eventFactory.Core().V1().Events().Informer()

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected pod object type: %T", obj)
				return
			}

			var registered bool
			for _, container := range pod.Spec.Containers {
				for _, env := range container.Env {
					if env.Name != "CODER_AGENT_TOKEN" {
						continue
					}

					token, err := p.resolveEnvValue(p.ctx, pod.Namespace, env)
					if err != nil {
						p.logger.Warn(p.ctx, "failed to resolve CODER_AGENT_TOKEN",
							slog.F("pod", pod.Name),
							slog.F("namespace", pod.Namespace),
							slog.Error(err))
						continue
					}
					if token == "" {
						continue
					}

					registered = true
					p.tc.setPodToken(pod.Name, token)

					// We don't want to add logs to workspaces that are already started!
					if !pod.CreationTimestamp.After(startTime) {
						continue
					}

					p.sendLog(pod.Name, token, agentsdk.Log{
						CreatedAt: time.Now(),
						Output:    fmt.Sprintf("🐳 %s: %s", newColor(color.Bold).Sprint("Created pod"), pod.Name),
						Level:     codersdk.LogLevelDebug,
					})
				}
			}
			if registered {
				p.logger.Info(p.ctx, "registered agent pod", slog.F("name", pod.Name), slog.F("namespace", pod.Namespace))
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected pod delete object type: %T", obj)
				return
			}

			tokens := p.tc.deletePodToken(pod.Name)
			for _, token := range tokens {
				p.sendLog(pod.Name, token, agentsdk.Log{
					CreatedAt: time.Now(),
					Output:    fmt.Sprintf("🗑️ %s: %s", newColor(color.Bold).Sprint("Deleted pod"), pod.Name),
					Level:     codersdk.LogLevelError,
				})
				p.sendDelete(token)
			}
			p.logger.Info(p.ctx, "unregistered agent pod", slog.F("name", pod.Name))
		},
	})
	if err != nil {
		return fmt.Errorf("register pod handler: %w", err)
	}

	_, err = replicaInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			replicaSet, ok := obj.(*appsv1.ReplicaSet)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected replica object type: %T", obj)
				return
			}

			// We don't want to add logs to workspaces that are already started!
			if !replicaSet.CreationTimestamp.After(startTime) {
				return
			}

			var registered bool
			for _, container := range replicaSet.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					if env.Name != "CODER_AGENT_TOKEN" {
						continue
					}

					token, err := p.resolveEnvValue(p.ctx, replicaSet.Namespace, env)
					if err != nil {
						p.logger.Warn(p.ctx, "failed to resolve CODER_AGENT_TOKEN",
							slog.F("replicaset", replicaSet.Name),
							slog.F("namespace", replicaSet.Namespace),
							slog.Error(err))
						continue
					}
					if token == "" {
						continue
					}

					registered = true
					p.tc.setReplicaSetToken(replicaSet.Name, token)

					p.sendLog(replicaSet.Name, token, agentsdk.Log{
						CreatedAt: time.Now(),
						Output:    fmt.Sprintf("🐳 %s: %s", newColor(color.Bold).Sprint("Queued pod from ReplicaSet"), replicaSet.Name),
						Level:     codersdk.LogLevelDebug,
					})
				}
			}
			if registered {
				p.logger.Info(p.ctx, "registered agent pod from ReplicaSet", slog.F("name", replicaSet.Name))
			}
		},
		DeleteFunc: func(obj interface{}) {
			replicaSet, ok := obj.(*appsv1.ReplicaSet)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected replica set delete object type: %T", obj)
				return
			}

			tokens := p.tc.deleteReplicaSetToken(replicaSet.Name)
			if len(tokens) == 0 {
				return
			}

			for _, token := range tokens {
				p.sendLog(replicaSet.Name, token, agentsdk.Log{
					CreatedAt: time.Now(),
					Output:    fmt.Sprintf("🗑️ %s: %s", newColor(color.Bold).Sprint("Deleted ReplicaSet"), replicaSet.Name),
					Level:     codersdk.LogLevelError,
				})
				p.sendDelete(token)
			}

			p.logger.Info(p.ctx, "unregistered ReplicaSet", slog.F("name", replicaSet.Name))
		},
	})
	if err != nil {
		return fmt.Errorf("register replicaset handler: %w", err)
	}

	_, err = eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event, ok := obj.(*corev1.Event)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected event object type: %T", obj)
				return
			}

			// We don't want to add logs to workspaces that are already started!
			if !event.CreationTimestamp.After(startTime) {
				return
			}

			var tokens []string
			switch event.InvolvedObject.Kind {
			case "Pod":
				tokens = p.tc.getPodTokens(event.InvolvedObject.Name)
			case "ReplicaSet":
				tokens = p.tc.getReplicaSetTokens(event.InvolvedObject.Name)
			}
			if len(tokens) == 0 {
				return
			}

			for _, token := range tokens {
				p.sendLog(event.InvolvedObject.Name, token, agentsdk.Log{
					CreatedAt: time.Now(),
					Output:    newColor(color.FgWhite).Sprint(event.Message),
					Level:     codersdk.LogLevelDebug,
				})
				p.logger.Info(p.ctx, "sending log", slog.F("pod", event.InvolvedObject.Name), slog.F("message", event.Message))
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register event handler: %w", err)
	}

	p.logger.Info(p.ctx, "listening for pod events",
		slog.F("coder_url", p.coderURL.String()),
		slog.F("namespace", namespace),
		slog.F("field_selector", p.fieldSelector),
		slog.F("label_selector", p.labelSelector),
	)
	podFactory.Start(p.stopChan)
	if podFactory != eventFactory {
		eventFactory.Start(p.stopChan)
	}

	// Track HasSynced functions for WaitForCacheSync
	p.hasSyncedFuncs = append(p.hasSyncedFuncs,
		podInformer.HasSynced,
		replicaInformer.HasSynced,
		eventInformer.HasSynced,
	)

	return nil
}

// WaitForCacheSync waits for all informer caches to sync.
// This is useful for testing to ensure informers are ready before creating resources.
func (p *podEventLogger) WaitForCacheSync(ctx context.Context) bool {
	return cache.WaitForCacheSync(ctx.Done(), p.hasSyncedFuncs...)
}

var sourceUUID = uuid.MustParse("cabdacf8-7c90-425c-9815-cae3c75d1169")

// loggerForToken returns a logger for the given pod name and agent token.
// If a logger already exists for the token, it's returned. Otherwise a new
// logger is created and returned. It assumes a lock to p.mutex is already being
// held.
func (p *podEventLogger) sendLog(resourceName, token string, log agentsdk.Log) {
	p.logCh <- agentLog{
		op:           opLog,
		resourceName: resourceName,
		agentToken:   token,
		log:          log,
	}
}

func (p *podEventLogger) sendDelete(token string) {
	p.logCh <- agentLog{
		op:         opDelete,
		agentToken: token,
	}
}

// Close stops the pod event logger and releases all resources.
// Close is idempotent and safe to call multiple times.
func (p *podEventLogger) Close() error {
	p.closeOnce.Do(func() {
		p.cancelFunc()
		close(p.stopChan)
		close(p.errChan)
	})
	// Wait for the work goroutine to exit
	<-p.doneChan
	return nil
}

type tokenCache struct {
	mu          sync.RWMutex
	pods        map[string][]string
	replicaSets map[string][]string
}

func (t *tokenCache) setPodToken(name, token string) []string { return t.set(t.pods, name, token) }
func (t *tokenCache) getPodTokens(name string) []string       { return t.get(t.pods, name) }
func (t *tokenCache) deletePodToken(name string) []string     { return t.delete(t.pods, name) }

func (t *tokenCache) setReplicaSetToken(name, token string) []string {
	return t.set(t.replicaSets, name, token)
}
func (t *tokenCache) getReplicaSetTokens(name string) []string { return t.get(t.replicaSets, name) }
func (t *tokenCache) deleteReplicaSetToken(name string) []string {
	return t.delete(t.replicaSets, name)
}

func (t *tokenCache) get(m map[string][]string, name string) []string {
	t.mu.RLock()
	tokens := m[name]
	t.mu.RUnlock()
	return tokens
}

func (t *tokenCache) set(m map[string][]string, name, token string) []string {
	t.mu.Lock()
	tokens, ok := m[name]
	if !ok {
		tokens = []string{token}
	} else {
		tokens = append(tokens, token)
	}
	m[name] = tokens
	t.mu.Unlock()

	return tokens
}

func (t *tokenCache) delete(m map[string][]string, name string) []string {
	t.mu.Lock()
	tokens := m[name]
	delete(m, name)
	t.mu.Unlock()
	return tokens
}

func (t *tokenCache) isEmpty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pods)+len(t.replicaSets) == 0
}

type op int

const (
	opLog op = iota
	opDelete
)

type agentLog struct {
	op           op
	resourceName string
	agentToken   string
	log          agentsdk.Log
}

// logQueuer is a single-threaded queue for dispatching logs.
type logQueuer struct {
	mu     sync.Mutex
	logger slog.Logger
	clock  quartz.Clock
	q      chan agentLog

	coderURL  *url.URL
	loggerTTL time.Duration
	loggers   map[string]agentLoggerLifecycle
	logCache  logCache

	// retries maps agent tokens to their retry state for exponential backoff
	retries map[string]*retryState
	// maxRetries is the maximum number of retries for a log send failure.
	maxRetries int
}

func (l *logQueuer) work(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer l.cleanup()

	for ctx.Err() == nil {
		select {
		case log := <-l.q:
			switch log.op {
			case opLog:
				l.processLog(ctx, log)
			case opDelete:
				l.processDelete(log)
			}

		case <-ctx.Done():
			return
		}

	}
}

// cleanup stops all retry timers and cleans up resources when the work loop exits.
func (l *logQueuer) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for token, rs := range l.retries {
		if rs != nil && rs.timer != nil {
			rs.timer.Stop()
		}
		delete(l.retries, token)
	}
}

func (l *logQueuer) newLogger(ctx context.Context, log agentLog) (agentLoggerLifecycle, error) {
	client := agentsdk.New(l.coderURL, agentsdk.WithFixedToken(log.agentToken))

	logger := l.logger.With(slog.F("resource_name", log.resourceName))
	client.SDK.SetLogger(logger)

	_, err := client.PostLogSource(ctx, agentsdk.PostLogSourceRequest{
		ID:          sourceUUID,
		Icon:        "/icon/k8s.png",
		DisplayName: "Kubernetes",
	})
	if err != nil {
		// Posting the log source failed, which affects how logs appear.
		// We'll retry to ensure the log source is properly registered.
		logger.Error(ctx, "post log source", slog.Error(err))
		return agentLoggerLifecycle{}, err
	}

	ls := agentsdk.NewLogSender(logger)
	sl := ls.GetScriptLogger(sourceUUID)

	gracefulCtx, gracefulCancel := context.WithCancel(context.Background())

	// Fetch build info to determine server capabilities.
	// The role query parameter was added in Coder v2.31.0. Servers at or
	// above this version support ConnectRPC28WithRole, which sends
	// role="logstream-kube" to skip connection monitoring (prevents false
	// agent connectivity state changes).
	buildInfo, buildInfoErr := client.SDK.BuildInfo(ctx)
	if buildInfoErr != nil {
		logger.Warn(ctx, "failed to get build info, falling back to ConnectRPC20", slog.Error(buildInfoErr))
	}
	supportsRole := buildInfoErr == nil && versionAtLeast(buildInfo.Version, "v2.31.0")

	var (
		logDest agentsdk.LogDest
		rpcConn drpc.Conn
	)
	if supportsRole {
		arpc, _, err := client.ConnectRPC28WithRole(gracefulCtx, "logstream-kube")
		if err != nil {
			logger.Error(ctx, "drpc connect with role", slog.Error(err))
			gracefulCancel()
			return agentLoggerLifecycle{}, err
		}
		logDest = arpc
		rpcConn = arpc.DRPCConn()
	} else {
		arpc, err := client.ConnectRPC20(gracefulCtx)
		if err != nil {
			logger.Error(ctx, "drpc connect", slog.Error(err))
			gracefulCancel()
			return agentLoggerLifecycle{}, err
		}
		logDest = arpc
		rpcConn = arpc.DRPCConn()
	}
	go func() {
		err := ls.SendLoop(gracefulCtx, logDest)
		// if the send loop exits on its own without the context
		// canceling, timeout the logger and force it to recreate.
		if err != nil && ctx.Err() == nil {
			l.loggerTimeout(log.agentToken)
		}
	}()

	closeTimer := l.clock.AfterFunc(l.loggerTTL, func() {
		logger.Info(ctx, "logger timeout firing")
		l.loggerTimeout(log.agentToken)
	})
	lifecycle := agentLoggerLifecycle{
		scriptLogger: sl,
		close: func() {
			defer func() {
				_ = rpcConn.Close()
			}()
			defer client.SDK.HTTPClient.CloseIdleConnections()
			// We could be stopping for reasons other than the timeout. If
			// so, stop the timer.
			closeTimer.Stop()
			defer gracefulCancel()
			timeout := l.clock.AfterFunc(5*time.Second, gracefulCancel)
			defer timeout.Stop()
			logger.Info(ctx, "logger closing")

			if err := sl.Flush(gracefulCtx); err != nil {
				// ctx err
				logger.Warn(gracefulCtx, "timeout reached while flushing")
				return
			}

			if err := ls.WaitUntilEmpty(gracefulCtx); err != nil {
				// ctx err
				logger.Warn(gracefulCtx, "timeout reached while waiting for log queue to empty")
			}
		},
	}
	lifecycle.closeTimer = closeTimer
	return lifecycle, nil
}

func (l *logQueuer) processLog(ctx context.Context, log agentLog) {
	l.mu.Lock()
	defer l.mu.Unlock()

	queuedLogs := l.logCache.get(log.agentToken)
	if isAgentLogEmpty(log) {
		if queuedLogs == nil {
			return
		}
	} else {
		queuedLogs = l.logCache.push(log)
	}

	lgr, ok := l.loggers[log.agentToken]
	if !ok {
		// skip if we're in a retry cooldown window
		if rs := l.retries[log.agentToken]; rs != nil && rs.timer != nil {
			return
		}

		var err error
		lgr, err = l.newLogger(ctx, log)
		if err != nil {
			l.scheduleRetry(ctx, log.agentToken)
			return
		}
		l.loggers[log.agentToken] = lgr
	}

	lgr.resetCloseTimer(l.loggerTTL)
	if len(queuedLogs) == 0 {
		return
	}
	if err := lgr.scriptLogger.Send(ctx, queuedLogs...); err != nil {
		l.scheduleRetry(ctx, log.agentToken)
		return
	}
	l.clearRetryLocked(log.agentToken)
	l.logCache.delete(log.agentToken)
}

func (l *logQueuer) processDelete(log agentLog) {
	l.mu.Lock()
	lgr, ok := l.loggers[log.agentToken]
	if ok {
		delete(l.loggers, log.agentToken)
	}
	l.clearRetryLocked(log.agentToken)
	l.logCache.delete(log.agentToken)
	l.mu.Unlock()

	if ok {
		// close this async, no one else will have a handle to it since we've
		// deleted from the map
		go lgr.close()
	}
}

func (l *logQueuer) loggerTimeout(agentToken string) {
	l.q <- agentLog{
		op:         opDelete,
		agentToken: agentToken,
	}
}

type agentLoggerLifecycle struct {
	scriptLogger agentsdk.ScriptLogger

	closeTimer *quartz.Timer
	close      func()
}

func (l *agentLoggerLifecycle) resetCloseTimer(ttl time.Duration) {
	if !l.closeTimer.Reset(ttl) {
		// If the timer had already fired and we made it active again, stop the
		// timer. We don't want it to run twice.
		l.closeTimer.Stop()
	}
}

// retryState tracks exponential backoff for an agent token.
type retryState struct {
	delay      time.Duration
	timer      *quartz.Timer
	retryCount int
	exhausted  bool // prevent retry state recreation after max retries
}

func (l *logQueuer) scheduleRetry(ctx context.Context, token string) {
	if l.retries == nil {
		l.retries = make(map[string]*retryState)
	}

	rs := l.retries[token]

	if rs != nil && rs.exhausted {
		return
	}

	if rs == nil {
		rs = &retryState{delay: time.Second, retryCount: 0, exhausted: false}
		l.retries[token] = rs
	}

	rs.retryCount++

	// If we've reached the max retries, clear the retry state and delete the log cache.
	if rs.retryCount >= l.maxRetries {
		l.logger.Error(ctx, "max retries exceeded",
			slog.F("retryCount", rs.retryCount),
			slog.F("maxRetries", l.maxRetries))
		rs.exhausted = true
		if rs.timer != nil {
			rs.timer.Stop()
			rs.timer = nil
		}
		l.logCache.delete(token)
		return
	}

	if rs.timer != nil {
		return
	}

	l.logger.Info(ctx, "scheduling retry",
		slog.F("delay", rs.delay.String()),
		slog.F("retryCount", rs.retryCount))

	rs.timer = l.clock.AfterFunc(rs.delay, func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		if cur := l.retries[token]; cur != nil && !cur.exhausted {
			cur.timer = nil
			l.q <- agentLog{op: opLog, agentToken: token}
		}
	})

	rs.delay *= 2
	if rs.delay > 30*time.Second {
		rs.delay = 30 * time.Second
	}
}

// clearRetryLocked clears the retry state for the given token.
// The caller must hold the mutex lock.
func (l *logQueuer) clearRetryLocked(token string) {
	if rs := l.retries[token]; rs != nil {
		if rs.timer != nil {
			rs.timer.Stop()
		}
		delete(l.retries, token)
	}
}

// versionAtLeast returns true if version is a valid semver string and is
// greater than or equal to minimum.
func versionAtLeast(version, minimum string) bool {
	return semver.IsValid(version) && semver.Compare(version, minimum) >= 0
}

func newColor(value ...color.Attribute) *color.Color {
	c := color.New(value...)
	c.EnableColor()
	return c
}

type logCache struct {
	logs map[string][]agentsdk.Log
}

func (l *logCache) push(log agentLog) []agentsdk.Log {
	logs, ok := l.logs[log.agentToken]
	if !ok {
		logs = make([]agentsdk.Log, 0, 1)
	}
	logs = append(logs, log.log)
	l.logs[log.agentToken] = logs
	return logs
}

func (l *logCache) delete(token string) {
	delete(l.logs, token)
}

func (l *logCache) get(token string) []agentsdk.Log {
	logs, ok := l.logs[token]
	if !ok {
		return nil
	}
	return logs
}

func isAgentLogEmpty(log agentLog) bool {
	return log.resourceName == "" && log.log.Output == "" && log.log.CreatedAt.IsZero()
}
