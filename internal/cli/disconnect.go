package cli

import (
	"context"
	"flag"
	"io"
	"strconv"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func parseDisconnect(args []string, out, errOut io.Writer) (*commonOptions, error) {
	options := &commonOptions{}
	set := flag.NewFlagSet("zoorikctl cluster disconnect", flag.ContinueOnError)
	commonFlags(set, options)
	if parsed, err := parseCommon(set, args, out, errOut, options); !parsed {
		return nil, err
	}
	return options, nil
}

func runDisconnect(ctx context.Context, args []string, s *session) error {
	options, err := parseDisconnect(args, s.streams.Out, s.streams.Err)
	if err != nil || options == nil {
		return err
	}

	out := s.out
	out.Banner("Zoorik Disconnect")
	target, err := kube.Load(options.Kubeconfig, options.KubeContext)
	if err != nil {
		return err
	}
	warnings := &kubeWarnings{out: out, seen: map[string]bool{}}
	target.OnWarning = warnings.add

	out.Section("Configuration")
	out.Success("Kubeconfig loaded.")
	out.Field("Kube context", target.ContextName)
	out.Field("Cluster", text.OrNone(target.ClusterName))
	out.Field("API server", target.ServerURL)
	out.Field("User", text.OrNone(target.UserName))
	out.Field("Kubeconfig", text.OrNone(target.Origin))

	if err := target.Connect(); err != nil {
		return err
	}
	occupant, _ := kube.InspectOccupant(ctx, target)
	identity, identifyErr := kube.Identify(ctx, target)

	out.Section("Plan")
	spinner := out.Spin("Reading what Zoorik holds on this cluster...")
	plan, err := kube.PlanRelease(ctx, target)
	warnings.flush()
	if err != nil {
		spinner.Fail("The cluster could not be read")
		return disconnectRefused(target, err, "Nothing was changed.")
	}
	spinner.Success("Read " + strconv.Itoa(len(plan.Items)) + " objects to release or remove")
	if plan.Empty() {
		out.Success("Nothing of Zoorik's is left on " + target.ContextName + ".")
		cleanUpNodeDeletion(ctx, s, target, identity, identifyErr, occupant, options)
		return nil
	}
	for _, item := range plan.Items {
		out.Item(item.Describe())
	}
	out.Blank()
	out.Paragraph("", "Running pods keep the resources they have now; each takes its template's values "+
		"at its next restart. Nothing that is not Zoorik's own is deleted.")
	if options.DryRun {
		cleanUpNodeDeletion(ctx, s, target, identity, identifyErr, occupant, options)
		out.Blank()
		out.Info("Dry run: nothing was changed.")
		return nil
	}

	out.Section("Disconnect")
	err = plan.Execute(ctx, target, func(item kube.ReleaseItem) {
		out.Success(item.Describe())
	}, func(item kube.ReleaseItem, reason string) {
		out.Warn(item.Describe() + ": " + reason)
	}, func(item kube.ReleaseItem) {
		out.Info("Found after the agent stopped: " + item.Describe())
	})
	warnings.flush()
	if err != nil {
		return disconnectRefused(target, err, "Every step already done stays done, and running the same "+
			"command again resumes where this one stopped.")
	}
	cleanUpNodeDeletion(ctx, s, target, identity, identifyErr, occupant, options)
	out.Blank()
	out.Success("Zoorik is disconnected from " + target.ContextName + ".")
	out.Paragraph("", "If this cluster is still listed in the console, removing it there no longer waits for "+
		"the agent: the console stops waiting once the agent has not been heard for "+
		strconv.Itoa(int(kube.ReleaseAgentSilence.Minutes()))+" minutes.")
	return nil
}

func disconnectRefused(target *kube.Target, err error, state string) error {
	code := refusal.CodeApply
	what := "The disconnect stopped: " + text.OneLine(err.Error())
	if apierrors.IsForbidden(err) {
		code = refusal.CodePermission
		what = "Your credentials may not make this change: " + text.OneLine(err.Error())
	}
	return refusal.Wrap(code, err, what, state+" `"+target.Kubectl("auth can-i --list")+"` shows what "+
		"your credentials may do.")
}
