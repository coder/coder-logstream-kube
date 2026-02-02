package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/sloghuman"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	cmd := root()
	err := cmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func root() *cobra.Command {
	var (
		coderURL      string
		fieldSelector string
		kubeConfig    string
		namespacesStr string
		labelSelector string
	)
	cmd := &cobra.Command{
		Use:   "coder-logstream-kube",
		Short: "Stream Kubernetes Pod events to the Coder startup logs.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if coderURL == "" {
				return fmt.Errorf("--coder-url is required")
			}
			parsedURL, err := url.Parse(coderURL)
			if err != nil {
				return fmt.Errorf("parse coder URL: %w", err)
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

			reporter, err := newPodEventLogger(cmd.Context(), podEventLoggerOptions{
				coderURL:      parsedURL,
				client:        client,
				namespaces:    namespaces,
				fieldSelector: fieldSelector,
				labelSelector: labelSelector,
				logger:        slog.Make(sloghuman.Sink(cmd.ErrOrStderr())).Leveled(slog.LevelDebug),
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
			case <-cmd.Context().Done():
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&coderURL, "coder-url", "u", os.Getenv("CODER_URL"), "URL of the Coder instance")
	cmd.Flags().StringVarP(&kubeConfig, "kubeconfig", "k", "~/.kube/config", "Path to the kubeconfig file")
	cmd.Flags().StringVarP(&namespacesStr, "namespaces", "n", os.Getenv("CODER_NAMESPACES"), "List of namespaces to use when listing pods")
	cmd.Flags().StringVarP(&fieldSelector, "field-selector", "f", "", "Field selector to use when listing pods")
	cmd.Flags().StringVarP(&labelSelector, "label-selector", "l", "", "Label selector to use when listing pods")

	return cmd
}
