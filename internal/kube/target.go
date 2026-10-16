package kube

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/refusal"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const shellWordPunctuation = "_@%+=:,./-"

type Target struct {
	ContextName string
	ClusterName string
	UserName    string
	ServerURL   string
	Origin      string
	OnWarning   func(string)

	kubeconfigPath string

	Dynamic   dynamic.Interface
	Clientset kubernetes.Interface
	Mapper    meta.RESTMapper

	restConfig *rest.Config
}

func Load(kubeconfigPath, contextName string) (*Target, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		rules.ExplicitPath = kubeconfigPath
	}

	raw, err := rules.Load()
	if err != nil {
		return nil, refusal.Wrap(refusal.CodeKubeconfig, err,
			"Your kubeconfig could not be read.",
			"Check the file named by $KUBECONFIG, or pass --kubeconfig with the path to use.")
	}

	resolved, err := resolveContext(raw, contextName)
	if err != nil {
		return nil, err
	}

	overrides := &clientcmd.ConfigOverrides{CurrentContext: resolved}
	config := clientcmd.NewDefaultClientConfig(*raw, overrides)

	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, refusal.Wrap(refusal.CodeKubeconfig, err,
			fmt.Sprintf("Context %q does not describe a cluster this command can dial.", resolved),
			"Run `kubectl config view --minify --context="+ShellWord(resolved)+"` and check that "+
				"the context names a server and credentials that still exist.")
	}
	restConfig.UserAgent = api.UserAgent()

	entry := raw.Contexts[resolved]
	target := &Target{
		ContextName:    resolved,
		ClusterName:    entry.Cluster,
		UserName:       entry.AuthInfo,
		ServerURL:      restConfig.Host,
		Origin:         originOf(entry, rules),
		kubeconfigPath: kubeconfigPath,
		restConfig:     restConfig,
	}
	restConfig.WarningHandlerWithContext = warningRelay{target: target}
	return target, nil
}

type warningRelay struct {
	target *Target
}

func (r warningRelay) HandleWarningHeaderWithContext(_ context.Context, code int, _ string,
	message string) {
	if code != 299 || message == "" || r.target.OnWarning == nil {
		return
	}
	r.target.OnWarning(message)
}

func (t *Target) KubeconfigPath() string {
	return t.kubeconfigPath
}

func (t *Target) UsesCredentialPlugin() bool {
	return t.restConfig != nil && (t.restConfig.ExecProvider != nil || t.restConfig.AuthProvider != nil)
}

func (t *Target) Kubectl(arguments string) string {
	command := "kubectl --context=" + ShellWord(t.ContextName)
	if t.kubeconfigPath != "" {
		command += " --kubeconfig=" + ShellWord(t.kubeconfigPath)
	}
	return command + " " + arguments
}

func ShellWord(value string) string {
	if value != "" && strings.IndexFunc(value, unsafeInShellWord) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func unsafeInShellWord(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune(shellWordPunctuation, r):
		return false
	}
	return true
}

func originOf(entry *clientcmdapi.Context, rules *clientcmd.ClientConfigLoadingRules) string {
	if entry != nil && entry.LocationOfOrigin != "" {
		return entry.LocationOfOrigin
	}
	if rules.ExplicitPath != "" {
		return rules.ExplicitPath
	}
	return strings.Join(rules.GetLoadingPrecedence(), string(filepath.ListSeparator))
}

func (t *Target) Connect() error {
	restConfig := t.restConfig
	if restConfig == nil {
		return refusal.New(refusal.CodeKubeconfig,
			"This command tried to reach a cluster it never resolved a context for.",
			"Report this: it is a defect in zoorikctl, not a problem with your kubeconfig.")
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return refusal.Wrap(refusal.CodeKubeconfig, err,
			fmt.Sprintf("Could not build a client for %s.", t.ServerURL), "Check the context's credentials.")
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return refusal.Wrap(refusal.CodeKubeconfig, err,
			fmt.Sprintf("Could not build a client for %s.", t.ServerURL), "Check the context's credentials.")
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return refusal.Wrap(refusal.CodeKubeconfig, err,
			fmt.Sprintf("Could not build a client for %s.", t.ServerURL), "Check the context's credentials.")
	}

	groups, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return refusal.Wrap(refusal.CodeKubeconfig, err,
			fmt.Sprintf("The cluster at %s did not answer a discovery request.", t.ServerURL),
			"This is the first call this command makes against the cluster. If it fails, the "+
				"context's server address or credentials are wrong -- nothing has been applied.")
	}

	t.Dynamic = dynamicClient
	t.Clientset = clientset
	t.Mapper = restmapper.NewDiscoveryRESTMapper(groups)
	return nil
}

func resolveContext(raw *clientcmdapi.Config, requested string) (string, error) {
	available := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		available = append(available, name)
	}
	sort.Strings(available)

	if requested != "" {
		if _, ok := raw.Contexts[requested]; !ok {
			return "", refusal.New(refusal.CodeKubeconfig,
				fmt.Sprintf("There is no context named %q in your kubeconfig.", requested),
				"Run `kubectl config get-contexts` and pass one of the names it lists to "+
					"--kube-context.").WithDetail(
				"Contexts found: " + strings.Join(available, ", "))
		}
		return requested, nil
	}

	if raw.CurrentContext == "" {
		return "", refusal.New(refusal.CodeKubeconfig,
			"Your kubeconfig has no current context, so there is no cluster to connect.",
			"Name the cluster with --kube-context=<name>, using one of the contexts below.").
			WithDetail("Contexts found: " + strings.Join(available, ", "))
	}
	if _, ok := raw.Contexts[raw.CurrentContext]; !ok {
		return "", refusal.New(refusal.CodeKubeconfig,
			fmt.Sprintf("Your kubeconfig's current context is %q, which is not defined in it.",
				raw.CurrentContext),
			"Name the cluster with --kube-context=<name>, using one of the contexts below.").
			WithDetail("Contexts found: " + strings.Join(available, ", "))
	}
	return raw.CurrentContext, nil
}
