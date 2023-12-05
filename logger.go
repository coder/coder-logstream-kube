package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"cdr.dev/slog"
	"github.com/coder/coder/codersdk"
	"github.com/coder/coder/codersdk/agentsdk"
	"github.com/fatih/color"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	// *Never* remove this. Certificates are not bundled as part
	// of the container, so this is necessary for all connections
	// to not be insecure.
	_ "github.com/breml/rootcerts"
)

type podEventLoggerOptions struct {
	client   kubernetes.Interface
	coderURL *url.URL

	logger      slog.Logger
	logDebounce time.Duration

	// The following fields are optional!
	namespace     string
	fieldSelector string
	labelSelector string
}

// newPodEventLogger creates a set of Kubernetes informers that listen for
// pods with containers that have the `CODER_AGENT_TOKEN` environment variable.
// Pod events are then streamed as startup logs to that agent via the Coder API.
func newPodEventLogger(ctx context.Context, opts podEventLoggerOptions) (*podEventLogger, error) {
	if opts.logDebounce == 0 {
		opts.logDebounce = 250 * time.Millisecond
	}
	ctx, cancelFunc := context.WithCancel(ctx)
	reporter := &podEventLogger{
		podEventLoggerOptions: &opts,
		stopChan:              make(chan struct{}),
		errChan:               make(chan error, 16),
		ctx:                   ctx,
		cancelFunc:            cancelFunc,
		agentTokenToLogger:    map[string]*agentLogger{},
		podToAgentTokens:      map[string][]string{},
		replicaSetToTokens:    map[string][]string{},
	}
	return reporter, reporter.init()
}

type podEventLogger struct {
	*podEventLoggerOptions

	stopChan chan struct{}
	errChan  chan error

	ctx                context.Context
	cancelFunc         context.CancelFunc
	mutex              sync.RWMutex
	agentTokenToLogger map[string]*agentLogger
	podToAgentTokens   map[string][]string
	replicaSetToTokens map[string][]string
}

// init starts the informer factory and registers event handlers.
func (p *podEventLogger) init() error {
	// We only track events that happen after the reporter starts.
	// This is to prevent us from sending duplicate events.
	startTime := time.Now()

	podFactory := informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithNamespace(p.namespace), informers.WithTweakListOptions(func(lo *v1.ListOptions) {
		lo.FieldSelector = p.fieldSelector
		lo.LabelSelector = p.labelSelector
	}))
	eventFactory := podFactory
	if p.fieldSelector != "" || p.labelSelector != "" {
		// Events cannot filter on labels and fields!
		eventFactory = informers.NewSharedInformerFactoryWithOptions(p.client, 0, informers.WithNamespace(p.namespace))
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
			p.mutex.Lock()
			defer p.mutex.Unlock()

			var registered bool
			for _, container := range pod.Spec.Containers {
				for _, env := range container.Env {
					if env.Name != "CODER_AGENT_TOKEN" {
						continue
					}
					registered = true
					tokens, ok := p.podToAgentTokens[pod.Name]
					if !ok {
						tokens = make([]string, 0)
					}
					tokens = append(tokens, env.Value)
					p.podToAgentTokens[pod.Name] = tokens

					// We don't want to add logs to workspaces that are already started!
					if !pod.CreationTimestamp.After(startTime) {
						continue
					}

					p.sendLog(pod.Name, env.Value, agentsdk.StartupLog{
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
			p.mutex.Lock()
			defer p.mutex.Unlock()
			tokens, ok := p.podToAgentTokens[pod.Name]
			if !ok {
				return
			}
			delete(p.podToAgentTokens, pod.Name)
			for _, token := range tokens {
				p.sendLog(pod.Name, token, agentsdk.StartupLog{
					CreatedAt: time.Now(),
					Output:    fmt.Sprintf("🗑️ %s: %s", newColor(color.Bold).Sprint("Deleted pod"), pod.Name),
					Level:     codersdk.LogLevelError,
				})
			}
			p.logger.Info(p.ctx, "unregistered agent pod", slog.F("name", pod.Name))
		},
	})
	if err != nil {
		return fmt.Errorf("register pod handler: %w", err)
	}

	_, err = replicaInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			replica, ok := obj.(*appsv1.ReplicaSet)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected replica object type: %T", obj)
				return
			}

			// We don't want to add logs to workspaces that are already started!
			if !replica.CreationTimestamp.After(startTime) {
				return
			}

			p.mutex.Lock()
			defer p.mutex.Unlock()

			var registered bool
			for _, container := range replica.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					if env.Name != "CODER_AGENT_TOKEN" {
						continue
					}
					registered = true
					tokens, ok := p.replicaSetToTokens[replica.Name]
					if !ok {
						tokens = make([]string, 0)
					}
					tokens = append(tokens, env.Value)
					p.replicaSetToTokens[replica.Name] = tokens

					p.sendLog(replica.Name, env.Value, agentsdk.StartupLog{
						CreatedAt: time.Now(),
						Output:    fmt.Sprintf("🐳 %s: %s", newColor(color.Bold).Sprint("Queued pod from ReplicaSet"), replica.Name),
						Level:     codersdk.LogLevelInfo,
					})
				}
			}
			if registered {
				p.logger.Info(p.ctx, "registered agent pod from ReplicaSet", slog.F("name", replica.Name))
			}
		},
		DeleteFunc: func(obj interface{}) {
			replicaSet, ok := obj.(*appsv1.ReplicaSet)
			if !ok {
				p.errChan <- fmt.Errorf("unexpected replica set delete object type: %T", obj)
				return
			}
			p.mutex.Lock()
			defer p.mutex.Unlock()
			_, ok = p.replicaSetToTokens[replicaSet.Name]
			if !ok {
				return
			}
			delete(p.replicaSetToTokens, replicaSet.Name)
			for _, pod := range replicaSet.Spec.Template.Spec.Containers {
				name := pod.Name
				if name == "" {
					name = replicaSet.Spec.Template.Name
				}
				tokens, ok := p.podToAgentTokens[name]
				if !ok {
					continue
				}
				delete(p.podToAgentTokens, name)
				for _, token := range tokens {
					p.sendLog(pod.Name, token, agentsdk.StartupLog{
						CreatedAt: time.Now(),
						Output:    fmt.Sprintf("🗑️ %s: %s", newColor(color.Bold).Sprint("Deleted ReplicaSet"), replicaSet.Name),
						Level:     codersdk.LogLevelError,
					})
				}
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

			p.mutex.Lock()
			defer p.mutex.Unlock()
			var tokens []string
			switch event.InvolvedObject.Kind {
			case "Pod":
				tokens, ok = p.podToAgentTokens[event.InvolvedObject.Name]
			case "ReplicaSet":
				tokens, ok = p.replicaSetToTokens[event.InvolvedObject.Name]
			}
			if tokens == nil || !ok {
				return
			}

			for _, token := range tokens {
				p.sendLog(event.InvolvedObject.Name, token, agentsdk.StartupLog{
					CreatedAt: time.Now(),
					Output:    newColor(color.FgWhite).Sprint(event.Message),
					Level:     codersdk.LogLevelInfo,
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
		slog.F("namespace", p.namespace),
		slog.F("field_selector", p.fieldSelector),
		slog.F("label_selector", p.labelSelector),
	)
	podFactory.Start(p.stopChan)
	if podFactory != eventFactory {
		eventFactory.Start(p.stopChan)
	}
	return nil
}

// loggerForToken returns a logger for the given pod name and agent token.
// If a logger already exists for the token, it's returned. Otherwise a new
// logger is created and returned.
func (p *podEventLogger) sendLog(resourceName, token string, log agentsdk.StartupLog) {
	p.mutex.Lock()
	logger, ok := p.agentTokenToLogger[token]
	p.mutex.Unlock()
	if !ok {
		client := agentsdk.New(p.coderURL)
		client.SetSessionToken(token)
		client.SDK.Logger = p.logger.Named(resourceName)
		sendLog, closer := client.QueueStartupLogs(p.ctx, p.logDebounce)

		logger = &agentLogger{
			sendLog: sendLog,
			closer:  closer,
			closeTimer: time.AfterFunc(p.logDebounce*5, func() {
				logger.closed.Store(true)
				// We want to have two close cycles for loggers!
				err := closer.Close()
				if err != nil {
					p.logger.Error(p.ctx, "close agent logger", slog.Error(err), slog.F("pod", resourceName))
				}
				p.mutex.Lock()
				delete(p.agentTokenToLogger, token)
				p.mutex.Unlock()
			}),
		}
		p.agentTokenToLogger[token] = logger
	}
	if logger.closed.Load() {
		// If the logger was already closed, we await the close before
		// creating a new logger. This is to ensure all loggers get sent in order!
		_ = logger.closer.Close()
		go p.sendLog(resourceName, token, log)
		return
	}
	// We make this 5x the debounce because it's low-cost to persist a few
	// extra loggers, and it can improve performance if a lot of logs are
	// being sent.
	logger.closeTimer.Reset(p.logDebounce * 5)
	logger.sendLog(log)
}

func (p *podEventLogger) Close() error {
	p.cancelFunc()
	close(p.stopChan)
	close(p.errChan)
	return nil
}

// agentLogger is a wrapper around the agent SDK logger that
// ensures logs are sent in order and not too frequently.
type agentLogger struct {
	sendLog    func(log agentsdk.StartupLog)
	closer     io.Closer
	closeTimer *time.Timer
	closed     atomic.Bool
}

func newColor(value ...color.Attribute) *color.Color {
	c := color.New(value...)
	c.EnableColor()
	return c
}
