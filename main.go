package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/sloghuman"
	"github.com/coder/serpent"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	cmd := root()
	err := cmd.Invoke().WithOS().Run()
	if err != nil {
		os.Exit(1)
	}
}

func root() *serpent.Command {
	var (
		coderURL      string
		fieldSelector string
		kubeConfig    string
		namespacesStr string
		labelSelector string
	)
	cmd := &serpent.Command{
		Use:   "coder-logstream-kube",
		Short: "Stream Kubernetes Pod events to the Coder startup logs.",
		Options: serpent.OptionSet{
			{
				Name:          "coder-url",
				Flag:          "coder-url",
				FlagShorthand: "u",
				Env:           "CODER_URL",
				Value:         serpent.StringOf(&coderURL),
				Description:   "URL of the Coder instance.",
			},
			{
				Name:          "kubeconfig",
				Flag:          "kubeconfig",
				FlagShorthand: "k",
				Default:       "~/.kube/config",
				Value:         serpent.StringOf(&kubeConfig),
				Description:   "Path to the kubeconfig file.",
			},
			{
				Name:          "namespaces",
				Flag:          "namespaces",
				FlagShorthand: "n",
				Env:           "CODER_NAMESPACES",
				Value:         serpent.StringOf(&namespacesStr),
				Description:   "List of namespaces to use when listing pods.",
			},
			{
				Name:          "field-selector",
				Flag:          "field-selector",
				FlagShorthand: "f",
				Value:         serpent.StringOf(&fieldSelector),
				Description:   "Field selector to use when listing pods.",
			},
			{
				Name:          "label-selector",
				Flag:          "label-selector",
				FlagShorthand: "l",
				Value:         serpent.StringOf(&labelSelector),
				Description:   "Label selector to use when listing pods.",
			},
		},
		Handler: func(inv *serpent.Invocation) error {
			if coderURL == "" {
				return fmt.Errorf("--coder-url is required")
			}
			parsedURL, err := url.Parse(coderURL)
			if err != nil {
				return fmt.Errorf("parse coder URL: %w", err)
			}
			if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
				return fmt.Errorf("CODER_URL must include http:// or https:// scheme, got: %q", coderURL)
			}

			if len(kubeConfig) > 0 && kubeConfig[0] == '~' {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("get user home dir: %w", err)
				}
				kubeConfig = home + kubeConfig[1:]
			}

			config, err := restclient.InClusterConfig()
			if errors.Is(err, restclient.ErrNotInCluster) {
				config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
			}
			if err != nil {
				return fmt.Errorf("build kubeconfig: %w", err)
			}

			client, err := kubernetes.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("create kubernetes client: %w", err)
			}

			var namespaces []string
			if namespacesStr != "" {
				namespaces = strings.Split(namespacesStr, ",")
				for i, namespace := range namespaces {
					namespaces[i] = strings.TrimSpace(namespace)
				}
			}

			reporter, err := newPodEventLogger(inv.Context(), podEventLoggerOptions{
				coderURL:      parsedURL,
				client:        client,
				namespaces:    namespaces,
				fieldSelector: fieldSelector,
				labelSelector: labelSelector,
				logger:        slog.Make(sloghuman.Sink(inv.Stderr)).Leveled(slog.LevelDebug),
				maxRetries:    15, // 15 retries is the default max retries for a log send failure.
			})
			if err != nil {
				return fmt.Errorf("create pod event reporter: %w", err)
			}
			defer func() {
				_ = reporter.Close()
			}()
			select {
			case err := <-reporter.errChan:
				return fmt.Errorf("pod event reporter: %w", err)
			case <-inv.Context().Done():
			}
			return nil
		},
	}

	return cmd
}
