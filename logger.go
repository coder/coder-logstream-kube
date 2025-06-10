package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"cdr.dev/slog"
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

	// The following fields are optional!
	namespaces    string
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
		},
	}

	return reporter, reporter.init()
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
}

// parseNamespaces parses the comma-separated namespaces string and returns a slice of namespace names.
// If the input is empty, it returns an empty slice indicating all namespaces should be watched.
func parseNamespaces(namespaces string) []string {
	if namespaces == "" {
		return []string{}
	}

	var result []string
	for _, ns := range strings.Split(namespaces, ",") {
		ns = strings.TrimSpace(ns)
		if ns != "" {
			result = append(result, ns)
		}
	}
	return result
}

// init starts the informer factory and registers event handlers.
func (p *podEventLogger) init() error {
	// We only track events that happen after the reporter starts.
	// This is to prevent us from sending duplicate events.
	startTime := time.Now()

	go p.lq.work(p.ctx)

	namespaceList := parseNamespaces(p.namespaces)

	// If no namespaces specified, watch all namespaces
	if len(namespaceList) == 0 {
		return p.initForNamespace("", startTime)
	}

	// Watch specific namespaces
	for _, namespace := range namespaceList {
		if err := p.initForNamespace(namespace, startTime); err != nil {
			return fmt.Errorf("init for namespace %s: %w", namespace, err)
		}
	}

	return nil
}

// initForNamespace initializes informers for a specific namespace.
// If namespace is empty, it watches all namespaces.
func (p *podEventLogger) initForNamespace(namespace string, startTime time.Time) error {
	var podFactory informers.SharedInformerFactory
	if namespace == "" {
		// Watch all namespaces
		podFactory = informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithTweakListOptions(func(lo *v1.ListOptions) {
			lo.FieldSelector = p.fieldSelector
			lo.LabelSelector = p.labelSelector
		}))
	} else {
		// Watch specific namespace
		podFactory = informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithNamespace(namespace), informers.WithTweakListOptions(func(lo *v1.ListOptions) {
			lo.FieldSelector = p.fieldSelector
			lo.LabelSelector = p.labelSelector
		}))
	}

	eventFactory := podFactory
	if p.fieldSelector != "" || p.labelSelector != "" {
		// Events cannot filter on labels and fields!
		if namespace == "" {
			eventFactory = informers.NewSharedInformerFactoryWithOptions(p.client, 0)
		} else {
			eventFactory = informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithNamespace(namespace))
		}
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
					registered = true
					p.tc.setPodToken(pod.Name, env.Value)

					// We don't want to add logs to workspaces that are already started!
					if !pod.CreationTimestamp.After(startTime) {
						continue
					}

					p.sendLog(pod.Name, env.Value, agentsdk.Log{
						CreatedAt: time.Now(),
						Output:    fmt.Sprintf("🐳 %s: %s", newColor(color.Bold).Sprint("Created pod"), pod.Name),
						Level:     codersdk.LogLevelInfo,
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
					registered = true
					p.tc.setReplicaSetToken(replicaSet.Name, env.Value)

					p.sendLog(replicaSet.Name, env.Value, agentsdk.Log{
						CreatedAt: time.Now(),
						Output:    fmt.Sprintf("📦 %s: %s", newColor(color.Bold).Sprint("Created replicaset"), replicaSet.Name),
						Level:     codersdk.LogLevelInfo,
					})
				}
			}
			if registered {
				p.logger.Info(p.ctx, "registered agent replicaset", slog.F("name", replicaSet.Name), slog.F("namespace", replicaSet.Namespace))
			}
		},
		DeleteFunc: func(obj interface{}) {
			replicaSet, ok := obj.(*appsv1.ReplicaSet)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected replicaset delete object type: %T", obj)
				return
			}

			tokens := p.tc.deleteReplicaSetToken(replicaSet.Name)
			for _, token := range tokens {
				p.sendLog(replicaSet.Name, token, agentsdk.Log{
					CreatedAt: time.Now(),
					Output:    fmt.Sprintf("🗑️ %s: %s", newColor(color.Bold).Sprint("Deleted replicaset"), replicaSet.Name),
					Level:     codersdk.LogLevelError,
				})
			}
			p.logger.Info(p.ctx, "unregistered agent replicaset", slog.F("name", replicaSet.Name))
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

			// We only care about events for pods and replicasets.
			var tokens []string
			switch event.InvolvedObject.Kind {
			case "Pod":
				tokens = p.tc.getPodTokens(event.InvolvedObject.Name)
			case "ReplicaSet":
				tokens = p.tc.getReplicaSetTokens(event.InvolvedObject.Name)
			default:
				return
			}

			if len(tokens) == 0 {
				return
			}

			level := codersdk.LogLevelInfo
			if event.Type == "Warning" {
				level = codersdk.LogLevelWarn
			}

			for _, token := range tokens {
				p.sendLog(event.InvolvedObject.Name, token, agentsdk.Log{
					CreatedAt: event.CreationTimestamp.Time,
					Output:    fmt.Sprintf("⚡ %s: %s", newColor(color.Bold).Sprint(event.Reason), event.Message),
					Level:     level,
				})
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register event handler: %w", err)
	}

	go podFactory.Start(p.ctx.Done())
	if eventFactory != podFactory {
		go eventFactory.Start(p.ctx.Done())
	}

	return nil
}

func (p *podEventLogger) sendLog(name, token string, log agentsdk.Log) {
	select {
	case p.logCh <- agentLog{
		name:  name,
		token: token,
		log:   log,
	}:
	case <-p.ctx.Done():
	}
}

func (p *podEventLogger) sendDelete(token string) {
	select {
	case p.logCh <- agentLog{
		token:  token,
		delete: true,
	}:
	case <-p.ctx.Done():
	}
}

func (p *podEventLogger) Close() error {
	p.cancelFunc()
	close(p.stopChan)
	return nil
}

type tokenCache struct {
	mu          sync.RWMutex
	pods        map[string][]string
	replicaSets map[string][]string
}

func (tc *tokenCache) setPodToken(name, token string) []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tokens, ok := tc.pods[name]
	if !ok {
		tc.pods[name] = []string{token}
		return []string{token}
	}

	for _, t := range tokens {
		if t == token {
			return append([]string(nil), tokens...)
		}
	}

	tc.pods[name] = append(tokens, token)
	return append([]string(nil), tc.pods[name]...)
}

func (tc *tokenCache) deletePodToken(name string) []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tokens, ok := tc.pods[name]
	if !ok {
		return nil
	}

	delete(tc.pods, name)
	return tokens
}

func (tc *tokenCache) getPodTokens(name string) []string {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	tokens, ok := tc.pods[name]
	if !ok {
		return nil
	}

	return append([]string(nil), tokens...)
}

func (tc *tokenCache) setReplicaSetToken(name, token string) []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tokens, ok := tc.replicaSets[name]
	if !ok {
		tc.replicaSets[name] = []string{token}
		return []string{token}
	}

	for _, t := range tokens {
		if t == token {
			return append([]string(nil), tokens...)
		}
	}

	tc.replicaSets[name] = append(tokens, token)
	return append([]string(nil), tc.replicaSets[name]...)
}

func (tc *tokenCache) deleteReplicaSetToken(name string) []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tokens, ok := tc.replicaSets[name]
	if !ok {
		return nil
	}

	delete(tc.replicaSets, name)
	return tokens
}

func (tc *tokenCache) getReplicaSetTokens(name string) []string {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	tokens, ok := tc.replicaSets[name]
	if !ok {
		return nil
	}

	return append([]string(nil), tokens...)
}

func (tc *tokenCache) isEmpty() bool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	return len(tc.pods) == 0 && len(tc.replicaSets) == 0
}

type agentLog struct {
	name   string
	token  string
	log    agentsdk.Log
	delete bool
}

type logQueuer struct {
	logger    slog.Logger
	clock     quartz.Clock
	q         <-chan agentLog
	coderURL  *url.URL
	loggerTTL time.Duration

	mu      sync.RWMutex
	loggers map[string]agentLoggerLifecycle

	logCache logCache
}

type logCache struct {
	mu   sync.RWMutex
	logs map[string][]agentsdk.Log
}

func (lc *logCache) append(token string, log agentsdk.Log) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	lc.logs[token] = append(lc.logs[token], log)
}

func (lc *logCache) flush(token string) []agentsdk.Log {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	logs, ok := lc.logs[token]
	if !ok {
		return nil
	}

	delete(lc.logs, token)
	return logs
}

type agentLoggerLifecycle struct {
	client *agentsdk.Client
	sourceID uuid.UUID
	timer  *quartz.Timer
	cancel context.CancelFunc
}

func (lq *logQueuer) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case log := <-lq.q:
			if log.delete {
				lq.deleteLogger(log.token)
				continue
			}

			lq.logCache.append(log.token, log.log)
			lq.ensureLogger(ctx, log.token)
		}
	}
}

func (lq *logQueuer) ensureLogger(ctx context.Context, token string) {
	lq.mu.Lock()
	defer lq.mu.Unlock()

	lifecycle, ok := lq.loggers[token]
	if ok {
		lifecycle.timer.Reset(lq.loggerTTL)
		return
	}

	coderClient := codersdk.New(lq.coderURL)
	coderClient.SetSessionToken(token)
	agentClient := agentsdk.New(lq.coderURL)
	agentClient.SetSessionToken(token)

	// Create a log source for this agent
	sourceID := agentsdk.ExternalLogSourceID
	_, err := agentClient.PostLogSource(ctx, agentsdk.PostLogSourceRequest{
		ID:          sourceID,
		DisplayName: "Kubernetes",
		Icon:        "/icon/k8s.png",
	})
	if err != nil {
		// Log source might already exist, which is fine
		lq.logger.Debug(ctx, "failed to create log source", slog.Error(err))
	}

	// Create a context for this logger that can be cancelled
	loggerCtx, cancel := context.WithCancel(ctx)

	timer := lq.clock.AfterFunc(lq.loggerTTL, func() {
		lq.deleteLogger(token)
	})

	lq.loggers[token] = agentLoggerLifecycle{
		client:   agentClient,
		sourceID: sourceID,
		timer:    timer,
		cancel:   cancel,
	}

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		
		for {
			select {
			case <-loggerCtx.Done():
				return
			case <-ticker.C:
				logs := lq.logCache.flush(token)
				if len(logs) == 0 {
					continue
				}

				err := agentClient.PatchLogs(loggerCtx, agentsdk.PatchLogs{
					LogSourceID: sourceID,
					Logs:        logs,
				})
				if err != nil {
					lq.logger.Error(loggerCtx, "patch agent logs", slog.Error(err))
					// Don't return on error, keep trying
				}
			}
		}
	}()
}

func (lq *logQueuer) deleteLogger(token string) {
	lq.mu.Lock()
	defer lq.mu.Unlock()

	lifecycle, ok := lq.loggers[token]
	if !ok {
		return
	}

	lifecycle.timer.Stop()
	lifecycle.cancel() // Cancel the context to stop the goroutine
	delete(lq.loggers, token)
}

func newColor(attrs ...color.Attribute) *color.Color {
	c := color.New(attrs...)
	c.EnableColor()
	return c
}
