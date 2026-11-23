package cli

import (
	"cmp"
	"context"
	"flag"
	"io"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/cloud"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"
	"github.com/getzoorik/zoorikctl/internal/ui"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const disconnectCanceled = "Disconnect canceled."

type unfinished struct {
	code refusal.Code
}

func (u *unfinished) Error() string {
	return "the disconnect did not finish"
}

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
	if err != nil {
		spinner.Fail("The cluster could not be read")
		warnings.flush()
		return disconnectRefused(target, err, "Nothing was changed.")
	}
	spinner.Success("Read what Zoorik holds on this cluster")
	warnings.flush()
	for _, item := range plan.Items {
		out.Item(item.Describe())
	}
	tells := !plan.Empty()
	if tells {
		out.Blank()
		out.Paragraph("", "Running pods keep the resources they have now; each takes its template's values "+
			"at its next restart. Nothing that is not Zoorik's own is deleted.")
	}
	undo := planCloudUndo(ctx, out, target, identity, identifyErr, occupant)
	if !tells && undo.clean() {
		out.Blank()
		nothingLeft(out, target)
		return nil
	}
	if options.DryRun {
		out.Blank()
		out.Info("Dry run: nothing was changed.")
		return nil
	}
	if len(undo.pending) > 0 {
		out.Blank()
		if err := askCloudUndo(ctx, s, target, &undo, options.Yes); err != nil {
			return err
		}
	}
	if !tells && !undo.agreed {
		out.Blank()
		printCloudLeft(out, undo, false)
		return &unfinished{code: undo.code()}
	}

	out.Section("Disconnect")
	report := func(item kube.ReleaseItem) {
		out.Halt()
		if item.Left {
			out.Warn(item.Describe())
			return
		}
		out.Success(item.Describe())
	}
	stopped := func(err error) error {
		out.Halt()
		warnings.flush()
		return disconnectRefused(target, err, "Every step already done stays done, and running the same "+
			"command again resumes where this one stopped.")
	}
	if err := plan.StopAgent(ctx, target, report); err != nil {
		return stopped(err)
	}
	var answer api.DisconnectAnswer
	if tells {
		answer = plan.TellZoorik(ctx)
		reportAnswer(out, plan, answer)
	}
	if undo.agreed {
		if err := applySteps(ctx, out, undo.plan, undo.pending); err != nil {
			if ctx.Err() != nil {
				return stopped(ctx.Err())
			}
			out.Warn("Undoing the cloud changes stopped: " + causeOf(err))
			undo.left = stillPending(ctx, undo.pending)
		}
	}
	if err := plan.RemoveActors(ctx, target, report); err != nil {
		return stopped(err)
	}
	if err := plan.Refresh(ctx, target, func(item kube.ReleaseItem) {
		out.Info("Found after the agent stopped: " + item.Describe())
	}); err != nil {
		return stopped(err)
	}
	if err := plan.HandBack(ctx, target, report); err != nil {
		return stopped(err)
	}
	var waiting *ui.Spinner
	stuck, err := plan.MovePools(ctx, target, func(item kube.ReleaseItem) {
		waiting = nil
		report(item)
	}, func(label string) {
		if waiting == nil {
			waiting = out.Spin(label)
			return
		}
		waiting.Tick(label)
	})
	out.Halt()
	if err != nil {
		return stopped(err)
	}
	keep := answer.Unreachable || !undo.done()
	if len(stuck) == 0 {
		if err := plan.DeleteDefinitions(ctx, target, report); err != nil {
			return stopped(err)
		}
		if !keep {
			uninstall := func(name, namespace string) error {
				return cloud.UninstallHelmRelease(target, name, namespace)
			}
			if err := plan.UninstallHelm(report, uninstall); err != nil {
				return stopped(err)
			}
			if err := plan.DeleteNamespace(ctx, target, report); err != nil {
				return stopped(err)
			}
		}
	}
	warnings.flush()

	out.Blank()
	var code refusal.Code
	if len(stuck) > 0 {
		printStopped(out, stuck)
		code = refusal.CodeApply
	}
	if !undo.done() {
		printCloudLeft(out, undo, keep && plan.HasNamespace())
		code = cmp.Or(code, undo.code())
	}
	finished := len(stuck) == 0 && undo.done()
	switch {
	case !tells:
		if undo.done() {
			nothingLeft(out, target)
		}
	case answer.Told && finished:
		out.Success("Zoorik is disconnected from " + target.ContextName + ".")
		out.Paragraph("    ", "The cluster has left the Zoorik console.")
	case answer.Told:
		out.Success("The cluster has left the Zoorik console.")
	case answer.Unreachable:
		out.Warn("Zoorik could not be reached at " + plan.ControlPlaneURL + ": " + answer.Reason + ".")
		stays := "The zoorik-agent namespace stays, holding the key Zoorik needs."
		if finished {
			stays = "Everything else is removed; the zoorik-agent namespace stays, holding the key Zoorik needs."
		}
		out.Paragraph("    ", stays+" Run this command again to take the cluster off the console.")
		code = cmp.Or(code, refusal.CodeControlPlane)
	default:
		out.Warn("Zoorik was not told: " + answer.Reason + ".")
		removed := ""
		if finished {
			removed = "Everything else is removed. "
		}
		out.Paragraph("    ", removed+"The cluster stays on the Zoorik console: connect it again, then run this "+
			"command, to take it off.")
		code = cmp.Or(code, refusal.CodeControlPlane)
	}
	if code == 0 {
		return nil
	}
	return &unfinished{code: code}
}

func askCloudUndo(ctx context.Context, s *session, target *kube.Target, undo *cloudUndo, yes bool) error {
	cluster := text.OrText(undo.plan.Cluster, target.ContextName)
	question := "Remove Zoorik's access to " + cluster + " in the cloud, with " + undo.plan.Credential + "? [Y/n]"
	if undo.plan.RestoresMinimums() {
		question = "Remove Zoorik's access to " + cluster + " in the cloud and put back the node pool minimums " +
			"it lowered, with " + undo.plan.Credential + "? [Y/n]"
	}
	err := askCloudChanges(ctx, s, undo.plan, len(undo.pending), yes, disconnectCanceled, true, question)
	switch {
	case err == nil:
		undo.agreed = true
		return nil
	case ctx.Err() != nil || !notAgreed(err):
		return err
	}
	undo.left = undo.pending
	undo.unasked = !s.streams.Interactive
	if undo.unasked {
		s.out.Warn("Nobody is at a terminal to agree, and --yes was not given: the cloud stays as it is; the " +
			"disconnect goes on in the cluster.")
		return nil
	}
	s.out.Warn("The cloud stays as it is; the disconnect goes on in the cluster.")
	return nil
}

func reportAnswer(out *ui.Printer, plan *kube.ReleasePlan, answer api.DisconnectAnswer) {
	zoorik := "Zoorik"
	if plan.ControlPlaneURL != "" {
		zoorik += " at " + plan.ControlPlaneURL
	}
	switch {
	case answer.Told:
		out.Success(zoorik + ": told; the cluster has left its console")
	case answer.Unreachable:
		out.Warn(zoorik + ": could not be reached: " + answer.Reason)
	default:
		out.Warn(zoorik + ": not told: " + answer.Reason)
	}
}

func printStopped(out *ui.Printer, stuck []kube.Stuck) {
	draining := false
	for _, item := range stuck {
		draining = draining || item.Draining
	}
	var lines []string
	if len(stuck) == 1 {
		out.Warn("The disconnect stopped: " + strings.TrimSuffix(stuck[0].What, ".") + ".")
		if stuck[0].Serving {
			lines = append(lines, "Its copies keep serving.")
		}
		lines = append(lines, "Fix that, then run this command again.")
	} else {
		out.Warn("The disconnect stopped:")
		for _, item := range stuck {
			what := strings.TrimSuffix(item.What, ".")
			if item.Serving {
				what += "; its copies keep serving"
			}
			out.Item(what)
		}
		lines = append(lines, "Fix these, then run this command again.")
	}
	if draining {
		lines = append(lines, "Emptying goes on without this command; a machine that a pod still holds stays, and "+
			"costs money, until that pod may leave.")
	}
	out.Paragraph("    ", strings.Join(lines, " "))
}

func printCloudLeft(out *ui.Printer, undo cloudUndo, namespace bool) {
	if undo.unread != "" {
		finish := "Fix that, then run this command again to finish."
		if namespace {
			finish = "Fix that, then run this command again to finish; the zoorik-agent namespace stays until then."
		}
		out.Warn("The cloud could not be read: " + undo.unread)
		out.Paragraph("    ", finish)
		return
	}
	out.Warn("Still to do in the cloud:")
	printSteps(out, undo.left)
	out.Blank()
	finish := "Run this command again and answer yes to finish."
	if undo.unasked {
		finish = "Run this command again with --yes to finish."
	}
	if namespace {
		finish = "The zoorik-agent namespace stays until then. " + finish
	}
	out.Paragraph("    ", finish)
}

func nothingLeft(out *ui.Printer, target *kube.Target) {
	out.Success("Nothing of Zoorik's is left in " + target.ContextName + ".")
	out.Paragraph("    ", "If the cluster still shows on the Zoorik console, connect it again, then run this command.")
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
