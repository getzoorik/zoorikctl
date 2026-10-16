package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/getzoorik/zoorikctl/internal/kube"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	helmkube "helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"helm.sh/helm/v3/pkg/strvals"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

const helmDriver = "secret"

type helmRelease struct {
	target    *kube.Target
	chart     string
	version   string
	release   string
	namespace string
	values    []string
	install   bool
	reuse     bool
	wait      bool
	timeout   time.Duration
}

func (h helmRelease) apply(ctx context.Context) error {
	settings := cli.New()
	values := map[string]any{}
	for _, value := range h.values {
		if err := strvals.ParseInto(value, values); err != nil {
			return err
		}
	}
	loaded, err := loadChart(settings, h.chart, h.version)
	if err != nil {
		return err
	}
	config := new(action.Configuration)
	if err := config.Init(h.getter(settings), h.namespace, helmDriver, func(string, ...any) {}); err != nil {
		return err
	}
	last, err := config.Releases.Last(h.release)
	missing := errors.Is(err, driver.ErrReleaseNotFound)
	if err != nil && !missing {
		return err
	}
	uninstalled := !missing && last.Info.Status == release.StatusUninstalled
	if missing || uninstalled {
		if !h.install {
			return fmt.Errorf("%q has no deployed releases", h.release)
		}
		install := action.NewInstall(config)
		install.ReleaseName = h.release
		install.Namespace = h.namespace
		install.Replace = uninstalled
		install.Wait = h.wait
		install.Timeout = h.timeout
		_, err := install.RunWithContext(ctx, loaded, values)
		return err
	}
	upgrade := action.NewUpgrade(config)
	upgrade.Namespace = h.namespace
	upgrade.ReuseValues = h.reuse
	upgrade.Wait = h.wait
	upgrade.Timeout = h.timeout
	upgrade.MaxHistory = settings.MaxHistory
	_, err = upgrade.RunWithContext(ctx, h.release, loaded, values)
	return err
}

func (h helmRelease) getter(settings *cli.EnvSettings) genericclioptions.RESTClientGetter {
	flags := genericclioptions.NewConfigFlags(false)
	*flags.KubeConfig = h.target.KubeconfigPath()
	*flags.Context = h.target.ContextName
	*flags.Namespace = h.namespace
	flags.WrapConfigFn = func(config *rest.Config) *rest.Config {
		config.Burst = settings.BurstLimit
		config.QPS = settings.QPS
		config.WarningHandler = rest.NoWarnings{}
		config.Wrap(func(base http.RoundTripper) http.RoundTripper {
			return &helmkube.RetryingRoundTripper{Wrapped: base}
		})
		return config
	}
	return flags
}

func loadChart(settings *cli.EnvSettings, ref, version string) (*chart.Chart, error) {
	client, err := registry.NewClient(registry.ClientOptEnableCache(true),
		registry.ClientOptCredentialsFile(settings.RegistryConfig))
	if err != nil {
		return nil, err
	}
	locate := action.NewInstall(&action.Configuration{RegistryClient: client})
	locate.Version = version
	path, err := locate.LocateChart(ref, settings)
	if err != nil {
		return nil, err
	}
	return loader.Load(path)
}

func readChart(ref, version string) error {
	_, err := loadChart(cli.New(), ref, version)
	return err
}

func helmRegistryConfig() string {
	return cli.New().RegistryConfig
}
