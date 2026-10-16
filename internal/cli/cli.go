package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/cloud"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/ui"
)

type Streams struct {
	In          io.Reader
	Out         io.Writer
	Err         io.Writer
	Interactive bool
}

type session struct {
	streams Streams
	out     *ui.Printer
	errs    *ui.Printer
}

func Main(ctx context.Context, args []string, streams Streams) int {
	log := openRunLog(args)
	out := ui.New(streams.Out).Tee(log.writer())
	defer out.Close()
	errs := ui.New(streams.Err).Tee(log.writer())
	defer errs.Close()

	err := run(ctx, args, &session{streams: streams, out: out, errs: errs})
	out.Halt()
	if err == nil {
		log.discard()
		return 0
	}

	var stopped *declined
	if errors.As(err, &stopped) {
		stopped.print(errs)
		log.discard()
		return int(refusal.CodeNotConfirmed)
	}

	code := refusal.ExitCode(err)
	report(errs, err)
	if code == int(refusal.CodeUsage) {
		log.discard()
		return code
	}
	if path := log.keep(err, code); path != "" {
		errs.Blank()
		errs.Paragraph("", "You can find logs to share with Zoorik support at "+path)
	}
	return code
}

func run(ctx context.Context, args []string, s *session) error {
	if len(args) == 0 {
		printUsage(s.streams.Out)
		return nil
	}

	switch args[0] {
	case "cluster":
		return runCluster(ctx, args[1:], s)
	case "version", "--version", "-v":
		return runVersion(s.streams)
	case "help", "--help", "-h":
		printUsage(s.streams.Out)
		return nil
	default:
		printUsage(s.streams.Err)
		return usageError("zoorikctl", fmt.Sprintf("%q is not a zoorikctl command.", args[0]))
	}
}

func runCluster(ctx context.Context, args []string, s *session) error {
	if len(args) == 0 {
		printUsage(s.streams.Err)
		return usageError("zoorikctl", "`zoorikctl cluster` needs a verb: `prepare`, `connect`, "+
			"`grant-node-deletion` or `disconnect`.")
	}
	switch args[0] {
	case "connect":
		return runConnect(ctx, args[1:], s)
	case "disconnect":
		return runDisconnect(ctx, args[1:], s)
	case "prepare":
		return runPrepare(ctx, args[1:], s)
	case "grant-node-deletion":
		return runGrant(ctx, args[1:], s)
	default:
		printUsage(s.streams.Err)
		return usageError("zoorikctl", fmt.Sprintf("`zoorikctl cluster %s` is not a command. The verbs are "+
			"`prepare`, `connect`, `grant-node-deletion` and `disconnect`.", args[0]))
	}
}

type commonOptions struct {
	KubeContext string
	Kubeconfig  string
	DryRun      bool
	Yes         bool
}

func commonFlags(set *flag.FlagSet, common *commonOptions) {
	set.StringVar(&common.KubeContext, "kube-context", "", "the kubeconfig context to act on")
	set.StringVar(&common.Kubeconfig, "kubeconfig", "", "kubeconfig file to read")
	set.BoolVar(&common.DryRun, "dry-run", false, "print the plan and exit")
	set.BoolVar(&common.Yes, "yes", false, "make the planned changes without asking")
	set.BoolVar(&common.Yes, "y", false, "the same as --yes")
}

func parseCommon(set *flag.FlagSet, args []string, out, errOut io.Writer, common *commonOptions) (bool, error) {
	set.SetOutput(errOut)
	set.Usage = func() {}
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(out)
			return false, nil
		}
		printUsage(errOut)
		return false, usageError(set.Name(), "That is not a flag zoorikctl understands.")
	}
	if set.NArg() > 0 {
		return false, usageError(set.Name(), fmt.Sprintf("`%s` takes no positional arguments, and was given %q.",
			set.Name(), set.Arg(0)))
	}
	common.KubeContext = strings.TrimSpace(common.KubeContext)
	return true, nil
}

func usageError(command, message string) error {
	return refusal.New(refusal.CodeUsage, message, "Run `"+command+" --help`.")
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `zoorikctl — prepare a Kubernetes cluster for Zoorik, connect it, grant node deletion, or disconnect it.

USAGE
  zoorikctl cluster prepare [--kube-context=<context>] [--cluster-resource-id=<arm-id|eks-arn>]
                            [--dry-run] [--yes]
  zoorikctl cluster connect --connect-token=<connect-token> --api-region=<region>
                            [--kube-context=<context>] [--yes] [--dry-run]
                            [--cluster-name=<name>] [--cluster-resource-id=<arm-id|eks-arn>]
                            [--cloud-identity-client-id=<client-id|role-arn>]
                            [--webhook-host-network]
  zoorikctl cluster disconnect [--kube-context=<context>] [--yes] [--dry-run]
  zoorikctl cluster grant-node-deletion [--kube-context=<context>] [--cluster-resource-id=<arm-id|eks-arn>]
                                        [--dry-run] [--yes]
  zoorikctl version

  Every command acts on your kubeconfig's current context, as kubectl and helm do, or on the one
  --kube-context names, and prints that context, its cluster, API server and kubeconfig first.

PREPARE
  Run by a cluster admin with their own cloud sign-in. It reads the cluster, says what provisions its
  nodes, reports every node pool's readiness for live migration, and plans what is missing: a node
  provisioner where the cluster has neither one nor a cluster autoscaler, and the provisioner's
  default NodePools where it has no NodePool of the cluster's own (Zoorik's zoorik-* pools do not
  count). It asks before changing anything. Zoorik only removes nodes: adding them for Pending pods
  is the job of the cluster's own provisioner or autoscaler, and Zoorik never works against it.
  prepare never creates a machine, never changes a node pool you own, and never holds a cloud
  credential: every cloud change runs through your own az or aws, and each is printed first.
  connect makes the same plan.

  What provisions the nodes:
    AKS   Node Auto Provisioning (AKS's managed Karpenter). If the cluster autoscaler runs on any
          node pool, nothing is changed. If NAP is off, prepare turns it on with its default
          NodePools, which add nodes for Pending pods. If it is on, its NodePools and other
          settings are kept, and where it has no NodePool of the cluster's own, prepare turns its
          default NodePools on. Either way prepare adds the node class zoorik-base (Azure Linux)
          that Zoorik's own zoorik-* pools clone. It refuses kubenet clusters, and names the
          Windows pools NAP does not provision.
    EKS   Karpenter. A running Karpenter or EKS Auto Mode is kept as it is, except where it has
          no NodePool of the cluster's own: then prepare adds Karpenter's default NodePool on the
          cluster's own EC2NodeClass (the one named default, if there is one; it stops and says
          what to create if there is none), or turns EKS Auto Mode's built-in NodePools
          general-purpose and system on. Beside a running Karpenter it adds the node class
          zoorik-base, copied from the cluster's own EC2NodeClass, and names a missing static
          capacity (turning it on where zoorikctl installed Karpenter), since Coral's rebalancing
          needs it. If a cluster autoscaler (its ConfigMap cluster-autoscaler-status, in any
          namespace) manages a node group, nothing is changed. Otherwise prepare creates the node
          role KarpenterNodeRole-<cluster> and its access entry, the controller role
          KarpenterControllerRole-<cluster> with Karpenter's cluster-scoped policy and an EKS
          Pod Identity association (adding the Pod Identity Agent add-on if missing), the
          interruption queue Karpenter-<cluster> with its four EventBridge rules, the
          karpenter.sh/discovery tag on the node groups' subnets and the cluster security group,
          the Karpenter `+cloud.DefaultKarpenterVersion+` Helm chart in kube-system with static capacity on
          (settings.featureGates.staticCapacity), which Coral's rebalancing pools need, the node class zoorik-base
          (Amazon Linux 2023) that Zoorik's own pools clone, and Karpenter's default EC2NodeClass
          and NodePool, which add on-demand c, m and r nodes for Pending pods. On a cluster that grants nodes access
          only through the aws-auth ConfigMap, it first switches the authentication mode to
          API_AND_CONFIG_MAP, which keeps every aws-auth mapping and cannot be switched back.
    Local clusters get the readiness report only.

  Each step is checked before it runs, so running prepare again changes nothing that is already
  set up, and a run that stopped part-way resumes where it stopped.

  The readiness report lists, per node pool, the OS image, kernel, container runtime, kubelet, CPU
  series and architecture, and whether live migration can run there: Linux on amd64 or arm64 with
  containerd, whatever the OS image, kernel or containerd release. Windows and Fargate nodes are named
  as unsupported, and nodes on containerd 2.4 or later as live-migration sources only; Zoorik never
  changes a node's containerd. Once the live-migration daemon runs, each node's own proof (the
  coral.zoorik.com/live-migration node annotation) decides, including whether the node is a
  source only: the CRIU placed at /usr/local/bin/criu hashes to the digest the daemon's image
  carries, and a short-lived Zoorik canary process was checkpointed from one pod and restored into a
  second through containerd there on the node's current CRIU, containerd and OS image. Where the
  cluster has no node provisioner, pools that are not ready are yours to move by hand; Zoorik never
  changes them.

  --kube-context     The kubeconfig context to prepare. Without it, the current context.
  --kubeconfig       Use this kubeconfig file instead of the default resolution.
  --cluster-resource-id
                     The AKS cluster's ARM id or the EKS cluster's ARN. Without it prepare finds
                     the AKS cluster from the nodes' resource group, and the EKS cluster from the
                     kubeconfig's cluster name or its API server address.
  --dry-run          Read the cluster and the cloud, print the plan with every command it would
                     run, and exit. Nothing is changed. Where az or aws is missing or not
                     signed in, it prints what provisions the nodes and the readiness report, names
                     what is missing, and exits 0.
  --yes, -y          Make the planned changes without asking. With no terminal to ask at, the
                     changes are made only with --yes.

CONNECT
  Fetches the install this connect token was opened for, shows what it will create and where, asks
  the cluster which cloud it runs on and which cluster it is, and applies it. A cluster whose nodes
  carry azure:// or aws:// provider ids is connected as an Azure or AWS cluster; any other, including
  one with no node yet, as a local cluster, which gets no cloud prices. The control plane learns
  which cluster this is when the agent reports. After applying, it deletes each ClusterRole,
  ClusterRoleBinding, Role and RoleBinding labelled app.kubernetes.io/part-of=zoorik-agent that the
  install no longer contains, such as a grant an earlier release installed, then each such
  ValidatingAdmissionPolicy and binding, then each such Service, ConfigMap, DaemonSet and
  CustomResourceDefinition the install no longer contains, and prints each one.
  Afterwards the agent follows each Zoorik release on its own: it moves its own images and
  applies its CRDs and roles, bounded by the admission policies zoorik-agent-release and
  zoorik-agent-release-images. It never widens a permission; a release that needs one waits for
  the next connect.

  The install carries the agent, the workload autoscaler's Recommendation resources, a pod
  webhook that sizes pods as they are created, and the Evictor; everything it changes on your own
  objects is released on disconnect. The webhook fails open, so it never blocks a pod.
  The connect token decides the organisation and whether live migration is installed; the install
  grants the agent what both engines need to act, and the console's policies, modes and engine
  switches decide what it actually does. Every object is applied as rendered.
  Before applying, it reads the cluster and adapts the install: it refuses Kubernetes older than
  1.30 and a cluster whose nodes are all neither amd64 nor arm64; on OpenShift it leaves the pods'
  user and group to OpenShift; and it sizes the agent's and the webhook's memory by the cluster's pod
  count. A cluster with no node yet is installed as rendered, and the agent starts on the first node
  that joins. After applying, it waits up to 3 minutes for the agent to start, reporting an image
  that cannot be pulled or a container that keeps restarting, then waits for the webhook and probes
  it through the API server.

  --connect-token    The connect token (zoorik_v1_...) from the Connect dialog. Required.
  --api-region       Which control plane to ask. Default "us".
  --api-url          A control plane address, instead of --api-region. For development.

  --kube-context     The kubeconfig context to install into. Without it, the current context.
  --yes, -y          Make the cloud changes below without asking. With no terminal and no --yes,
                     the install goes ahead and the cloud changes are left to prepare and
                     grant-node-deletion.
  --kubeconfig       Use this kubeconfig file instead of the default resolution.
  --cluster-name     The name the console shows for this cluster. Defaults to the kubeconfig's
                     cluster entry. An AKS cluster whose node resource group is the managed
                     MC_<resource-group>_<name>_<region> one is shown under its AKS name instead.
  --cluster-resource-id
                     The cluster's own id at its cloud: an AKS managed cluster's ARM id,
                     /subscriptions/<subscription>/resourceGroups/<resource-group>/providers/
                     Microsoft.ContainerService/managedClusters/<name>, or an EKS cluster's ARN,
                     arn:aws:eks:<region>:<account>:cluster/<name>. The console shows the
                     cluster's project (resource group) and cloud name from it; an AKS cluster
                     with a custom node resource group needs it for that. Checked against the
                     nodes before anything is applied. A later run keeps it, after checking it
                     against the nodes again.
  --cloud-identity-client-id
                     The identity the agent removes drained nodes with: an Azure user-assigned
                     managed identity's client id (a GUID), or an IAM role ARN for IRSA. It is
                     written to the agent's service account's azure.workload.identity/client-id
                     or eks.amazonaws.com/role-arn annotation. A later run keeps it.

  Cloud setup. On AKS or EKS, connect plans the cloud side with your own az or aws sign-in before
  it installs, prints every change and asks two questions, one for each part:
    Node provisioning, prepare's plan: where the cluster has no provisioner and no cluster
          autoscaler, Node Auto Provisioning with its default NodePools on AKS and Karpenter with a
          default NodePool on EKS; where NAP, Karpenter or EKS Auto Mode has no NodePool of the
          cluster's own (Zoorik's zoorik-* pools do not count), its default NodePools. A
          provisioner's other NodePools and settings, and a cluster autoscaler, are kept as they
          are. It asks "Make these <n> change(s) to <cluster> with <sign-in>? [y/N]".
    Removing spare machines and adjusting node pool sizes, so the agent can delete a machine it has
          cordoned and emptied itself, and lower a node pool's minimum when that minimum alone keeps
          such a machine, never below 1 on an AKS system pool or 0 elsewhere. It never adds a machine
          and never raises a minimum or maximum. It asks "Let Zoorik remove spare machines and adjust
          node pool sizes in <cluster>, with <sign-in>? [Y/n]", and Enter answers yes:
    AKS   turns on workload identity and the OIDC issuer if they are off; creates the managed identity
          zoorik-agent-<cluster> in the cluster's resource group with the federated credential
          zoorik-agent for zoorik-agent/zoorik-agent; defines the role "Zoorik Node Removal v2" in the
          subscription (read the cluster, its agent pools and machines; delete machines; write agent
          pools, the narrowest Azure action that lowers a pool's minimum count, which also allows
          creating or changing agent pools), or makes the
          tenant's existing one assignable there, and assigns it on this cluster only. An identity
          that holds the earlier "Zoorik Node Removal v1" on the cluster gets v2 and then loses that
          v1 assignment; the v1 role stays defined.
    EKS   creates the IAM role zoorik-agent-<cluster> (zoorik-agent-<cluster>-<region> when another
          cluster holds that name) whose inline policy ZoorikNodeRemoval-<cluster>-<hash> may describe
          Auto Scaling groups and terminate an instance only in a group tagged for this cluster;
          update such a group's minimum size, which IAM cannot separate from its desired capacity and
          other group settings, though it refuses a request that carries a maximum, a launch
          configuration or template, an image, subnets, instance types or capacity reservations;
          describe this cluster's managed node groups and update their configuration (minimum,
          maximum, desired size, labels and taints), which AWS cannot narrow further; and
          write only the tags coral.zoorik.com/original-min and coral.zoorik.com/lowered-min on those
          groups. It binds the role with EKS Pod Identity (adding the Pod Identity Agent add-on if
          missing; the role's trust admits only zoorik-agent/zoorik-agent on this cluster). A role
          that holds an earlier version of the policy gets this one.
    Before it lowers a minimum, the agent records the one you set in the tag
    coral.zoorik.com/original-min (coral.zoorik.com_original-min on AKS); disconnect puts it back.
    If you raise a minimum the agent lowered, it never lowers that pool again.
  An identity or role that only zoorik-agent/zoorik-agent on this cluster can use is reused; one that
  anything else can use as well gets no node removal, and the agent gets its own. A role or identity
  of zoorikctl's name that is not tagged for this cluster, or a role definition of that name that
  allows anything else, is never used. Zoorik never creates a machine or raises a node pool's size,
  and each step is checked first, so running it again changes nothing. If az or aws is missing or
  not signed in, or you answer no, connect still installs and prints the commands that finish it
  later:
    zoorikctl cluster prepare --kube-context=<context>
    zoorikctl cluster grant-node-deletion --kube-context=<context>
  Answering no to the second question leaves node removal as it was: node pools a provisioner or
  the cluster autoscaler manages are consolidated, and fixed-size ones are not. On a cluster that
  already holds the earlier machine-only grant, no keeps that grant bound as before, prints
  "already set up", and only node pool minimums stay as they are. While the node
  provisioning part is unfinished, connect ends with "Zoorik is connected, but the cloud setup is
  not finished" instead of "Zoorik setup complete!".
  --cloud-identity-client-id leaves the second part out: with it, connect asks no second question
  and makes no cloud change for it. A local cluster gets neither part.

  Live migration. A connect token opened with live migration adds the live-migration daemon:
  DaemonSet zoorik-node in the namespace zoorik-node, a privileged pod on every node that places one
  file, a static CRIU Zoorik builds from pinned upstream source, at /usr/local/bin/criu (no packages
  are installed) and can checkpoint and restore containers. Each node proves itself before it is
  used: a short-lived canary process in the zoorik-node namespace is checkpointed from one pod and
  restored into a second, at daemon start and again when the node's CRIU, containerd or OS image
  changes (a failed proof is retried after about 1 to 36 minutes). Connect prints what it does
  before anything is applied. A later connect whose token was opened without it removes it.

  --webhook-host-network
                     Run the pod webhook on the nodes' host network, listening on port 10263,
                     for a control plane that cannot reach pod IPs (EKS with an overlay CNI, some
                     hosted control planes). It sets the zoorik-agent namespace's Pod Security
                     level to privileged, so it is never a default: connect suggests it when its
                     probe times out. Not supported on OpenShift.

  --dry-run          Read the cluster and the cloud, print the cloud plan with every command it
                     would run and the exact agent setup that would be applied, and exit. Nothing
                     is changed. The grants a connect would delete are not listed; it prints the
                     commands that list them.

DISCONNECT
  Undoes what Zoorik changed on objects it does not own, then removes the install, and prints each
  object as it goes. It needs only your credentials and works whether or not the agent still runs.
  In order it: scales the zoorik-agent Deployment to 0 and waits for its pods to stop; deletes the
  zoorik-agent-webhook MutatingWebhookConfiguration, so no pod is mutated or held up by it; deletes
  the ClusterRoles, ClusterRoleBindings, Roles and RoleBindings labelled
  app.kubernetes.io/part-of=zoorik-agent, then its ValidatingAdmissionPolicyBindings and policies,
  before any namespace goes, so no policy is left waiting for a parameter that is gone; removes
  the live-migration daemon's namespace zoorik-node, if it is there; removes every
  coral.zoorik.com/ taint, the Evictor's annotations and the live-migration readiness annotation
  from the nodes; holds each HorizontalPodAutoscaler Zoorik created at its current replicas and
  deletes it, restores each one Zoorik took over to its recorded bounds, and reverts each
  conversion; restores each VerticalPodAutoscaler Zoorik took over to its recorded update policy,
  leaving alone one whose mode someone else has since changed; scales back a workload left one replica above its base by an
  interrupted rollout; deletes every Recommendation and ActionRequest and both resource definitions;
  and deletes the zoorik-agent namespace. Running pods keep
  their current resources. Every step can be run again: a disconnect that stops part-way resumes
  when the same command is run again. Offboard the cluster in the console as well.
  Last, it looks with your own az or aws sign-in for the cloud changes zoorikctl made to grant
  node deletion on this cluster. First, each node pool minimum the agent lowered goes back to the
  value in its tag coral.zoorik.com/original-min (coral.zoorik.com_original-min on AKS: an AKS
  agent pool, an EKS managed node group, or an Auto Scaling group tagged for this cluster), never
  above the pool's maximum now and never below the minimum it has now, and both coral.zoorik.com
  tags are removed; a minimum put back may add machines up to it. Then the
  objects it created, found by name and the zoorik-cluster tag: on AKS the "Zoorik Node Removal v2"
  and "Zoorik Node Removal v1" assignments on the cluster, then the federated credential and the
  managed identity zoorik-agent-<cluster>; on EKS the inline policy
  ZoorikNodeRemoval-<cluster>-<hash>, also on a role the agent used that zoorikctl did not create,
  then the pod identity association and the IAM role zoorik-agent-<cluster>. A cluster with no
  node that names its cloud is looked up by the cluster resource or identity the agent's
  configuration holds. It prints them and asks "Undo these <n> cloud change(s) zoorikctl made for
  <cluster>, with <sign-in>? [y/N]"; --yes undoes them, and with no terminal and no --yes they are
  left and their commands printed. An identity or role zoorikctl did not create keeps everything
  else it holds, and the roles "Zoorik Node Removal v2" and "Zoorik Node Removal v1", workload
  identity, the OIDC issuer and the Pod Identity Agent add-on stay. The node provisioner is the
  cluster's own and stays.

  --kube-context     The kubeconfig context to disconnect. Without it, the current context.
  --yes, -y          Undo the cloud changes zoorikctl made to grant node deletion, the lowered
                     minimums included, without asking.
  --kubeconfig       Use this kubeconfig file instead of the default resolution.
  --dry-run          Read the cluster and list every object that would be released or removed, and
                     the cloud changes it would offer to undo, and exit. Nothing is changed.

GRANT-NODE-DELETION
  Asks connect's second question for a cluster that is already connected, with no connect token:
  may Zoorik remove spare machines and adjust node pool sizes in this cluster. It reads the cluster
  and your own az or aws sign-in, prints every change, and asks "Let Zoorik remove spare machines
  and adjust node pool sizes in <cluster>, with <sign-in>? [Y/n]" before making any; Enter answers
  yes. It then binds the identity to the installed agent (the azure.workload.identity/client-id or
  eks.amazonaws.com/role-arn annotation of its service account; EKS Pod Identity needs neither) and
  restarts the agent so its credentials are injected. On a cluster whose grant predates node pool
  sizes, it moves the identity to "Zoorik Node Removal v2" on AKS or puts the new inline policy on
  EKS; until then the agent deletes machines only, as before. Running it again changes nothing. It
  sets up no node provisioner; prepare does. A local cluster has no machine to remove, so nothing
  is set up there.

  --kube-context     The kubeconfig context. Without it, the current context.
  --kubeconfig       Use this kubeconfig file instead of the default resolution.
  --cluster-resource-id
                     The AKS cluster's ARM id or the EKS cluster's ARN, when neither the nodes nor
                     the installed agent name it. It must be the cluster the context reaches (its
                     API server or its nodes are compared), and the agent's configuration is
                     corrected to it.
  --dry-run          Read the cluster and the cloud, print the plan with every command, and exit.
  --yes, -y          Make the changes without asking. With no terminal they are made only with --yes.

EXIT CODES
  0 connected   2 usage        3 credential     4 control plane   5 kubeconfig
  6 permission  7 wrong cluster
  8 apply refused, or a cloud plan blocked or stopped
  9 applied but agent did not start
  10 cloud changes not agreed to: the question was declined, or with no terminal --yes was not given
                (7 also refuses Kubernetes older than 1.30, a cluster whose nodes are all neither
                amd64 nor arm64, and a --cluster-resource-id that names another cluster)

`)
}
