package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/setup"
	"github.com/getzoorik/zoorikctl/internal/text"
	"github.com/getzoorik/zoorikctl/internal/ui"
)

const (
	webhookProbeAttempts = 3

	webhookProbePause = 5 * time.Second
)

func adaptToPlatform(ctx context.Context, out *ui.Printer, target *kube.Target,
	docs []setup.Document, options *connectOptions) error {
	platform, err := kube.DetectPlatform(ctx, target)
	if err != nil {
		out.Fail("This cluster cannot run the Zoorik agent")
		return err
	}
	out.Success("Kubernetes " + platform.VersionText())
	if len(platform.Architectures) == 0 {
		out.Info("No node yet: the agent and the pod webhook start on the first " +
			strings.Join(kube.SupportedArchitectures, " or ") + " node that joins")
	} else {
		out.Success("Node architectures: " + platform.ArchitectureText())
	}
	if len(platform.Unsupported) > 0 {
		out.Warn("Some nodes run on an architecture the agent image is not built for: " +
			platform.UnsupportedText())
		out.Detail("The agent and the pod webhook run only on " +
			strings.Join(kube.SupportedArchitectures, " or ") + " nodes. Pods on the other nodes are " +
			"still reported and sized.")
	}
	if platform.OpenShift {
		out.Success("OpenShift detected")
		if options.WebhookHostNetwork {
			return refusal.New(refusal.CodeUsage,
				kube.WebhookHostNetworkFlag+" is not supported on OpenShift: its security context "+
					"constraints admit a pod on the host network only through a constraint Zoorik does "+
					"not grant itself.",
				"Run it again without "+kube.WebhookHostNetworkFlag+". Nothing has been applied.")
		}
	}

	adaptation, err := platform.Adapt(docs)
	if err != nil {
		return err
	}
	for _, name := range adaptation.OpenShiftIdentity {
		out.Info("Deployment/" + name + ": fixed user, group and fsGroup ids removed, so OpenShift " +
			"assigns them; it still runs as non-root")
	}
	switch {
	case !platform.PodsCounted:
		out.Warn("Pods could not be counted, so the agent and the pod webhook keep their base memory")
	case platform.BeyondTiers:
		out.Warn(fmt.Sprintf("%d pods is above the largest size tier; the agent and the pod webhook "+
			"get its memory (%s and %s)", platform.Pods, platform.Tier.AgentMemory,
			platform.Tier.WebhookMemory))
	default:
		out.Success(fmt.Sprintf("%d pod(s): size tier up to %d pods (agent %s, pod webhook %s)",
			platform.Pods, platform.Tier.MaxPods, platform.Tier.AgentMemory,
			platform.Tier.WebhookMemory))
	}
	for _, resized := range adaptation.Resized {
		out.Log("    " + resized)
	}
	return nil
}

func printWebhookPlan(out *ui.Printer, summary setup.Summary, hostNetwork []string) {
	for _, object := range summary.Objects {
		if object.Kind != "MutatingWebhookConfiguration" || object.Name != kube.WebhookConfiguration {
			continue
		}
		out.Info("Adds the pod webhook " + object.String() + ": it sizes pods as they are created, " +
			"and fails open, so it never blocks a pod")
	}
	if len(hostNetwork) == 0 {
		return
	}
	out.Warn(kube.WebhookHostNetworkFlag + ": the pod webhook runs on the nodes' host network")
	for _, change := range hostNetwork {
		out.Log("    " + change)
	}
}

func waitForWebhook(ctx context.Context, out *ui.Printer, target *kube.Target,
	options *connectOptions) error {
	spinner := out.Spin("Waiting for the pod webhook to start...")
	waitCtx, cancel := context.WithTimeout(ctx, agentWait+30*time.Second)
	defer cancel()
	state, err := kube.WaitForWebhook(waitCtx, target, agentWait, spinner.Note)
	if err != nil {
		spinner.Fail("The pod webhook could not be watched")
		return err
	}
	if !state.Ready {
		spinner.Warn("The pod webhook is not ready")
		out.Detail("Reason: " + state.Reason + ". It fails open: until it is ready, pods are created " +
			"at their own size and nothing is blocked. Nothing needs to be re-applied; `" +
			target.Kubectl("-n "+kube.AgentNamespace+" describe deploy/"+kube.WebhookWorkload) +
			"` says what it is waiting for.")
		return nil
	}
	spinner.Success("Pod webhook is running (" + state.Pod + ")")
	probeWebhook(ctx, out, target, options)
	return nil
}

func probeWebhook(ctx context.Context, out *ui.Printer, target *kube.Target,
	options *connectOptions) {
	spinner := out.Spin("Probing the pod webhook through the API server...")
	var result kube.ProbeResult
	for attempt := 1; ; attempt++ {
		probed, err := kube.ProbeWebhook(ctx, target)
		if err != nil {
			spinner.Warn("The pod webhook could not be probed")
			out.Detail("Reason: " + text.OneLine(err.Error()) + ". The webhook probes itself every minute " +
				"and parks itself when the API server cannot reach it, so pod creation is never slowed.")
			return
		}
		result = probed
		if result.Answered() || attempt == webhookProbeAttempts {
			break
		}
		spinner.Note("no answer yet: " + result.Cause)
		select {
		case <-ctx.Done():
			spinner.Warn("The pod webhook probe was interrupted")
			return
		case <-time.After(webhookProbePause):
		}
	}
	if result.Answered() {
		spinner.Success("Pod webhook reachable (probe answered by " + result.AnsweredBy + ")")
		return
	}
	spinner.Warn("The pod webhook did not answer the API server's probe")
	out.Detail("Cause: " + result.Cause + ".")
	switch {
	case result.TimedOut && !options.WebhookHostNetwork:
		out.Detail(fmt.Sprintf("Remedy: run this command again with %s. It moves the pod webhook onto "+
			"the nodes' host network, port %d, which a control plane that cannot reach pod IPs (EKS "+
			"with an overlay CNI, some hosted control planes) can reach. It also sets the %s "+
			"namespace's Pod Security level to %s, so it is your choice rather than a default.",
			kube.WebhookHostNetworkFlag, kube.WebhookHostNetworkPort, kube.AgentNamespace,
			kube.PodSecurityPrivileged))
	case result.TimedOut:
		out.Detail(fmt.Sprintf("Remedy: allow the control plane to reach the nodes on TCP port %d.",
			kube.WebhookHostNetworkPort))
	default:
		out.Detail("If this persists, `" + target.Kubectl("-n "+kube.AgentNamespace+" logs deploy/"+
			kube.WebhookWorkload) + "` has the reason.")
	}
	out.Detail("It fails open, and until it answers it parks itself after three failed probes a minute " +
		"apart, so no pod create waits on it from then on; running pods are still resized in place, and new " +
		"pods are sized again as soon as it is reachable.")
}
