package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/cloud"
	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"
	"github.com/getzoorik/zoorikctl/internal/ui"
	"github.com/getzoorik/zoorikctl/internal/vm"
)

type vmConnectOptions struct {
	commonOptions
	ConnectToken string
	APIRegion    string
	APIURL       string
}

const (
	connectTokenVariable = "ZOORIK_CONNECT_TOKEN"

	vmGrantCanceled = "Not agreed: this run makes no change to Zoorik's access to this VM's data disks."

	agentConnected = "CONNECTED"

	accessGranted = "GRANTED"
)

type vmGrant struct {
	plan    cloud.VMDisks
	block   string
	unread  bool
	agreed  bool
	applied bool
}

func (g vmGrant) inPlace() bool {
	return !g.unread && (g.applied || len(g.plan.Pending()) == 0)
}

func (g vmGrant) shell() string {
	if g.plan.Cloud == kube.CloudAWS {
		return "AWS CloudShell"
	}
	return "Azure Cloud Shell (bash)"
}

func runVM(ctx context.Context, args []string, s *session) error {
	if len(args) == 0 {
		printUsage(s.streams.Err)
		return usageError("zoorikctl", "`zoorikctl vm` needs a verb: `connect`.")
	}
	if args[0] != "connect" {
		printUsage(s.streams.Err)
		return usageError("zoorikctl", fmt.Sprintf("`zoorikctl vm %s` is not a command. The verb is `connect`.",
			args[0]))
	}
	return vmReported(runVMConnect(ctx, args[1:], s))
}

func parseVMConnect(args []string, out, errOut io.Writer) (*vmConnectOptions, error) {
	options := &vmConnectOptions{}
	set := flag.NewFlagSet("zoorikctl vm connect", flag.ContinueOnError)
	set.StringVar(&options.ConnectToken, "connect-token", "", "connect token from the Connect a VM dialog")
	set.StringVar(&options.APIRegion, "api-region", "us", "which control plane to ask")
	set.StringVar(&options.APIURL, "api-url", "", "control plane address, instead of --api-region")
	set.BoolVar(&options.DryRun, "dry-run", false, "print the plan and exit")
	set.BoolVar(&options.Yes, "yes", false, "make the cloud changes without asking")
	set.BoolVar(&options.Yes, "y", false, "the same as --yes")
	if parsed, err := parseCommon(set, args, out, errOut, &options.commonOptions); !parsed {
		return nil, err
	}
	options.ConnectToken = strings.TrimSpace(text.OrText(options.ConnectToken, os.Getenv(connectTokenVariable)))
	if options.ConnectToken == "" {
		return nil, usageError(set.Name(), "No connect token was given. Run the two lines from the console's "+
			"Connect a VM dialog: the second passes the zoorik_v1_ connect token.")
	}
	return options, nil
}

func runVMConnect(ctx context.Context, args []string, s *session) error {
	options, err := parseVMConnect(args, s.streams.Out, s.streams.Err)
	if err != nil || options == nil {
		return err
	}

	out := s.out
	out.Banner("Zoorik VM Connect")
	amoeba, err := api.ResolveAmoebaURL(options.APIRegion, options.APIURL)
	if err != nil {
		return err
	}
	out.Section("Configuration")
	if strings.TrimSpace(options.APIURL) == "" {
		out.Field("Zoorik Region", options.APIRegion)
	}
	out.Field("Control plane", amoeba)

	out.Section("Preflight Checks")
	spinner := out.Spin("Reading this VM from its cloud's metadata service...")
	machine, err := vm.Preflight(ctx)
	if err != nil {
		spinner.Fail("This machine cannot be connected")
		return err
	}
	spinner.Success(machineSummary(machine))

	out.Section("API Verification")
	spinner = out.Spin("Verifying the connect token...")
	client := api.New(amoeba+api.AmoebaAgentPath, options.ConnectToken)
	setup, err := client.FetchVMSetup(ctx)
	if err != nil {
		spinner.Fail("Connect token could not be verified")
		return err
	}
	spinner.Success("Connect token verified")
	out.Plain("  Connecting this VM to " + setup.OrganizationName)
	printMachine(out, machine)

	out.Section("Data Disk Access")
	grant := planVMGrant(ctx, out, machine, setup)
	if err := settleVMGrantPlan(ctx, out, machine, setup, &grant); err != nil {
		return err
	}

	if options.DryRun {
		printVMBlock(out, grant)
		out.Section("Dry Run")
		out.Success("Nothing was changed: no sudo, no registration and no cloud change, and the connect token " +
			"is not spent. Run the command without --dry-run to connect this VM.")
		return nil
	}

	if pending := grant.plan.Pending(); !grant.unread && len(pending) > 0 {
		out.Section("Confirm")
		err := askCloudChanges(ctx, s, grant.plan.Plan, len(pending), options.Yes, vmGrantCanceled, true,
			"Let Zoorik change this VM's data disks, with "+grant.plan.Credential+"? [Y/n]")
		switch {
		case err == nil:
			grant.agreed = true
		case ctx.Err() != nil || !notAgreed(err):
			return err
		default:
			out.Warn(whatOf(err))
			out.Detail(grant.plan.Later())
		}
	}

	if err := installAgent(ctx, s, machine, client, setup); err != nil {
		return err
	}

	if grant.agreed {
		out.Section("Grant Data Disk Access")
		if err := applySteps(ctx, out, grant.plan.Plan, grant.plan.Pending()); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			printVMBlock(out, grant)
			return vmGrantFailed(err)
		}
		grant.applied = true
	}

	out.Section("Wait For Agent")
	spinner = out.Spin("Waiting for the agent's first report...")
	status, err := waitForVM(ctx, client.FetchVMConnectStatus, grant.inPlace(), agentWait, redemptionPollInterval,
		func(line string) { spinner.Tick(line + "...") })
	if err != nil {
		spinner.Fail("The agent has not reported")
		if !grant.inPlace() {
			printVMBlock(out, grant)
		}
		return err
	}
	spinner.Success("The agent reported")

	lines := []ui.BoxLine{{Text: "Console: " + amoeba + "/overview", Indent: 2}}
	switch {
	case status.Access == accessGranted:
		out.SuccessBox("Connected: "+machine.Name, lines)
		return nil
	case grant.inPlace():
		out.SuccessBox("Connected: "+machine.Name, lines)
		out.Info(providerName(machine.Cloud) + " is still applying the permission; this VM leaves Waiting for " +
			"permission on its own, usually within 10 minutes.")
		return nil
	}
	printVMBlock(out, grant)
	out.WarnBox("Connected: "+machine.Name, lines)
	return &declined{
		title: "This VM reads Waiting for permission until those commands run.",
		next:  "Its metrics are collected meanwhile, and no migration starts until Zoorik has that access.",
	}
}

func planVMGrant(ctx context.Context, out *ui.Printer, machine *vm.Machine, setup *api.VMSetup) vmGrant {
	runner := &cloud.Runner{As: vm.CLIUser(), OnRun: func(command cloud.Command) { out.Log("  $ " + command.String()) }}
	spinner := out.Spin("Reading what Zoorik may already do with this VM's data disks...")
	defer spinner.Stop()
	if machine.Cloud == kube.CloudAzure {
		azure := cloud.AzureVM{Subscription: machine.Subscription, ResourceGroup: machine.ResourceGroup,
			Name: machine.Name, ID: machine.ResourceID, DataDisks: machine.DataDisks}
		return vmGrant{
			plan:  cloud.PlanAzureVMDisks(ctx, runner, azure, setup.Grant.Azure, setup.OrganizationID),
			block: cloud.AzureCloudShell(azure, setup.Grant.Azure, setup.OrganizationID),
		}
	}
	aws := cloud.AWSVM{InstanceID: machine.InstanceID, Name: machine.Name, Region: machine.Region,
		Account: machine.Account}
	return vmGrant{
		plan:  cloud.PlanAWSVMDisks(ctx, runner, aws, setup.Grant.AWS),
		block: cloud.AWSCloudShell(aws, setup.Grant.AWS),
	}
}

func settleVMGrantPlan(ctx context.Context, out *ui.Printer, machine *vm.Machine, setup *api.VMSetup,
	grant *vmGrant) error {
	plan := &grant.plan
	if plan.TaggedFor != "" {
		return refusal.New(refusal.CodeWrongCluster,
			"The resource group "+machine.ResourceGroup+" is tagged "+setup.Grant.Azure.OrgTagKey+"="+
				plan.TaggedFor+", for another Zoorik organisation.",
			"A resource group belongs to one Zoorik organisation. Connect this VM from that organisation; if it "+
				"no longer uses the group, remove the tag, then run this again. Nothing has been changed.")
	}
	if len(plan.Blockers) > 0 && !plan.ToolingBlocked() {
		return planBlocked(plan.Plan)
	}
	if plan.ToolingBlocked() {
		for _, blocker := range plan.Blockers {
			out.Warn(blocker.What)
			out.Detail(blocker.Remedy)
		}
		grant.unread = true
		return nil
	}
	if err := checkSteps(ctx, out, &plan.Plan); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out.Warn(whatOf(err))
		out.Detail(plan.Later())
		grant.unread = true
		return nil
	}
	printSetupPlan(out, plan.Plan)
	return nil
}

func installAgent(ctx context.Context, s *session, machine *vm.Machine, client *api.Client,
	setup *api.VMSetup) error {
	out := s.out
	out.Section("Install Amoeba Agent")
	if os.Geteuid() != 0 {
		out.Info("Checking sudo, which writes the agent's settings and runs its install script as root")
	}
	if err := vm.CheckSudo(s.streams.In, s.streams.Err); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return refusal.Wrap(refusal.CodePermission, err, "sudo was refused, so the Amoeba agent cannot be installed.",
			"Run this as a user who may use sudo on this VM. Nothing has been changed, and the connect token is "+
				"not spent.")
	}

	spinner := out.Spin("Registering this VM...")
	attestation, err := vm.Attest(ctx, machine, setup.AttestationNonce)
	if err != nil {
		spinner.Fail("This VM's identity could not be read")
		return err
	}
	osName, osVersion := vm.OSRelease()
	details := api.VMDetails{Cloud: machine.Cloud, OSName: osName, OSVersion: osVersion, Attestation: attestation}
	if machine.Cloud == kube.CloudAzure {
		details.VMResourceID, details.VMName, details.VMSize = machine.ResourceID, machine.Name, machine.Size
		details.Region, details.Zone = machine.Region, machine.Zone
		details.OSOffer, details.OSSku = machine.OSOffer, machine.OSSku
	} else if machine.NameTagged {
		details.VMName = machine.Name
	}
	registered, err := client.RegisterVM(ctx, details, setup.ExpiresAt)
	if err != nil {
		spinner.Fail("This VM could not be registered")
		return err
	}
	if registered.Outcome == "RECONNECTED" {
		spinner.Success("Reconnected: a new agent key replaces the one this VM had")
	} else {
		spinner.Success("Registered with " + setup.OrganizationName)
	}

	if err := vm.WriteEnvFile(registered); err != nil {
		return refusal.Wrap(refusal.CodeAgentNotReady, err,
			"The agent's key and settings could not be written to "+vm.EnvFile+".",
			"Run the two lines from a new Connect a VM dialog; a rerun replaces this registration.")
	}
	out.Success("Wrote the agent's key and settings to " + vm.EnvFile + ", readable by root only")

	out.Info("Running the agent's install script as root:")
	code, last, err := vm.RunInstallScript(setup.InstallScript, out.Dim)
	if err != nil {
		return refusal.Wrap(refusal.CodeAgentNotReady, err, "The agent's install script could not be started.",
			"Run the two lines from a new Connect a VM dialog; a rerun replaces this registration.")
	}
	if code != 0 {
		return refusal.New(refusal.CodeAgentNotReady,
			fmt.Sprintf("The agent's install script stopped with exit code %d.", code),
			"Nothing in your cloud account was changed. Fix what the script's last lines name, then run the two "+
				"lines from a new Connect a VM dialog.").WithDetail(last...)
	}
	out.Success("Amoeba agent installed: " + vm.AgentUnit)
	return nil
}

func waitForVM(ctx context.Context, fetch func(context.Context) (*api.VMConnectStatus, error), granted bool,
	timeout, every time.Duration, progress func(string)) (*api.VMConnectStatus, error) {
	deadline := time.Now().Add(timeout)
	var last *api.VMConnectStatus
	var lastErr error
	for {
		status, err := fetch(ctx)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, api.ErrStatusRefused):
			return nil, err
		case err != nil:
			lastErr = err
		default:
			lastErr = nil
			if last == nil || *status != *last {
				progress(vmStatusLine(status))
			}
			last = status
			if status.Agent == agentConnected && (!granted || status.Access == accessGranted) {
				return status, nil
			}
		}
		if time.Now().After(deadline) {
			if last != nil && last.Agent == agentConnected {
				return last, nil
			}
			return nil, vmAgentTimedOut(timeout, last, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

func vmAgentTimedOut(timeout time.Duration, last *api.VMConnectStatus, lastErr error) error {
	failed := refusal.New(refusal.CodeAgentNotReady,
		fmt.Sprintf("The agent is installed but has not reported within %s.", timeout),
		"It keeps trying. Look for this VM in the console; if it does not show as connected within a few "+
			"minutes, its log says why: `sudo journalctl -u "+vm.AgentUnit+"`.")
	if last != nil {
		failed = failed.WithDetail("Last status: " + vmStatusLine(last))
	}
	if lastErr != nil {
		failed = failed.WithDetail("Last status check: " + whatOf(lastErr))
	}
	return failed
}

func vmStatusLine(status *api.VMConnectStatus) string {
	return "agent " + strings.ToLower(strings.ReplaceAll(text.OrText(status.Agent, "unknown"), "_", " ")) +
		", access " + strings.ToLower(text.OrText(status.Access, "unknown"))
}

func vmGrantFailed(err error) error {
	var failed *refusal.Refusal
	if !errors.As(err, &failed) {
		return err
	}
	return refusal.Wrap(failed.Code, failed.Cause, failed.What,
		"The agent is installed, and every step already done stays done. Run the commands above, or run the "+
			"two lines from a new Connect a VM dialog once your sign-in may make these changes.").
		WithDetail(failed.Detail...)
}

func vmReported(err error) error {
	var failed *refusal.Refusal
	if !errors.As(err, &failed) || failed.Cause == nil {
		return err
	}
	return refusal.New(failed.Code, failed.What, failed.Next).WithKind(failed.Kind).WithDetail(failed.Detail...).
		WithDetail("Reason: " + text.OneLine(failed.Cause.Error()))
}

func printVMBlock(out *ui.Printer, grant vmGrant) {
	out.Section("Cloud Shell Commands")
	out.Detail("These commands grant " + grant.plan.Provisioner + ". Paste them into " + grant.shell() + ", or give " +
		"them to someone who may make these changes; they are safe to run more than once.")
	out.Blank()
	out.Raw(grant.block)
	out.Log(grant.block)
}

func machineSummary(machine *vm.Machine) string {
	if machine.Cloud == kube.CloudAzure {
		return fmt.Sprintf("An Azure Linux VM with systemd and %d data disk(s)", len(machine.DataDisks))
	}
	return "An AWS EC2 Linux instance with systemd and NVMe EBS data volumes"
}

func printMachine(out *ui.Printer, machine *vm.Machine) {
	out.Field("VM", machine.Name)
	out.Field("Cloud", providerName(machine.Cloud))
	if machine.Cloud == kube.CloudAzure {
		out.Field("Subscription", machine.Subscription)
		out.Field("Resource group", machine.ResourceGroup)
	} else {
		out.Field("Account", machine.Account)
		out.Field("Instance", machine.InstanceID)
	}
	out.Field("Region", machine.Region)
}
