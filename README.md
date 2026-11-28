<p align="center">
  <img alt="Zoorik. Reduce cloud cost, autonomously." src=".github/brand/zoorik-reveal-tagline-banner-on-navy-960x240.gif" width="100%">
</p>

# zoorikctl

`zoorikctl` connects a Kubernetes cluster to [Zoorik Coral](https://zoorik.com/coral), Zoorik's
Kubernetes optimization. It prepares the cluster, installs the Zoorik agent, lets Zoorik remove spare
machines if you agree, and takes everything out again on disconnect. It also connects a Linux VM on
Azure or AWS to Zoorik Amoeba, Zoorik's block storage optimization. It is open source, so you can read
exactly what it does before you run it.

## Install

| | |
|---|---|
| macOS | `brew tap getzoorik/tap && brew trust getzoorik/tap && brew install zoorikctl` or `curl -fsSL https://get.zoorik.com/macos \| sh` |
| Linux | `curl -fsSL https://get.zoorik.com/linux \| sh` |
| Windows | `scoop bucket add getzoorik https://github.com/getzoorik/scoop-bucket && scoop install zoorikctl` or `irm https://get.zoorik.com/windows.ps1 \| iex` |

Binaries for macOS, Linux and Windows on amd64 and arm64 are on the
[Releases](https://github.com/getzoorik/zoorikctl/releases) page.

Every installer checks the archive against the release's `checksums.txt` before it writes anything.
When `cosign` is installed, it also checks that `checksums.txt` was signed by Zoorik's release
workflow. To check a release by hand:

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/getzoorik/(coral/\.github/workflows/zoorikctl-release|zoorikctl/\.github/workflows/release)\.yaml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

This command matches every release from v0.4.2 on. Up to v0.4.4, releases were built outside this
repository; from the next release on, they are built here by
[`release.yaml`](.github/workflows/release.yaml).

## Use it

The Connect dialog in the Zoorik console prints the command to run, with a one-time connect token:

```sh
zoorikctl cluster connect --connect-token=<token> --api-region=us
```

To connect a VM, run the two lines from the console's Connect a VM dialog on the VM itself:

```sh
curl -fsSL https://get.zoorik.com/linux | sudo sh
zoorikctl vm connect --connect-token="<token>" --api-region="us"
```

| Command | What it does |
|---|---|
| `zoorikctl cluster prepare` | Reads the cluster, says what adds its nodes, reports whether each node pool is ready for live migration, and sets up a node provisioner where the cluster has none. |
| `zoorikctl cluster connect` | Installs the Zoorik agent with the install the connect token was opened for. |
| `zoorikctl cluster grant-node-deletion` | Lets Zoorik remove machines it has emptied, and lower a node pool's minimum. |
| `zoorikctl cluster disconnect` | Undoes what Zoorik changed and removes the install. |
| `zoorikctl vm connect` | Connects the Linux VM it runs on to Zoorik Amoeba: installs the Amoeba agent and, if you agree, gives Zoorik access to the VM's data disks. |
| `zoorikctl version` | Prints the version. |

Every cluster command acts on your kubeconfig's current context, or on `--kube-context` (or `--context`, as
in kubectl), and prints that context first. `--dry-run` prints the full plan, with every command it would run, and changes nothing.
Every change is printed before it is made, and nothing in your cloud account changes until you say yes
(or pass `--yes`). Run `zoorikctl --help` for every flag and exit code.

## What it changes

### In the cluster

`connect` applies the install it downloads from Zoorik, as rendered:

- the namespace `zoorik-agent`, with the agent, a pod webhook that sizes pods as they are created, and
  the Evictor. The webhook fails open, so it never blocks a pod;
- the `Recommendation` and `ActionRequest` resource definitions, and the roles the agent needs, all
  labelled `app.kubernetes.io/part-of=zoorik-agent`;
- two admission policies, `zoorik-agent-release` and `zoorik-agent-release-images`, that bound how the
  agent updates itself. It never widens its own permissions;
- only when the connect token was opened with live migration: the DaemonSet `zoorik-node` in the
  namespace `zoorik-node`. It is privileged, and places one file on each node, a static CRIU at
  `/usr/local/bin/criu`. It installs no packages.

`zoorikctl` itself asks Zoorik only for the install, and sends nothing about your cluster. The agent
it installs reports to Zoorik once it runs. On EKS, `prepare` also pulls the Karpenter Helm chart from
`public.ecr.aws`.

### On a VM

`vm connect` registers the VM with the identity document its cloud signs for it, and with its name,
size, region and OS. It writes the agent's key and settings to `/etc/amoeba-agent.env`, readable by
root only, and runs the agent's install script from Zoorik as root. The script installs Java 17 if it
is missing, the agent as the systemd service `amoeba-agent`, and the timer `amoeba-agent-watchdog`,
which restarts the agent when it stops reporting. If a new version does not start, it rolls back to
the previous one, and the agent upgrades itself the same way. `sudo` is used for nothing else.

### In your cloud account

Cloud changes run through your own `az` or `aws` sign-in. `zoorikctl` never stores a cloud credential.

- **Node provisioning** (`prepare`, and the first question in `connect`). Where the cluster has no
  provisioner, or only the cluster autoscaler: on AKS it turns the autoscaler off on each pool that
  uses it (the pool keeps its nodes) and turns on Node Auto Provisioning, after which Azure refuses the
  autoscaler; on EKS it installs Karpenter with its IAM roles, interruption queue and discovery tags,
  and, where nodes join only through the `aws-auth` ConfigMap, switches the authentication mode to
  `API_AND_CONFIG_MAP`, which cannot be switched back. On EKS it then adds an admission policy that
  refuses the cluster autoscaler coming back and scales it to 0, so a Terraform or Helm apply that
  reinstates it fails with a message instead of running two autoscalers. On both it deletes the
  autoscaler's `cluster-autoscaler-status` ConfigMap. Where the switch cannot work (for example
  kubenet or Calico network policy on AKS), the autoscaler stays and `prepare` says why. A provisioner you already
  run is kept: `prepare` only adds a default NodePool where it has none of the cluster's own, and the
  node class `zoorik-base` that Zoorik's own pools copy.
- **Removing spare machines** (`grant-node-deletion`, and the second question in `connect`). On AKS: it
  turns on workload identity and the OIDC issuer if they are off, and creates a managed identity
  `zoorik-agent-<cluster>` with the custom role "Zoorik Node Removal v2" on this cluster only, and the
  read-only role "Zoorik Spot Capacity Reader v1" on the subscription, which reads a region's spot
  vCPU quota and how likely spot capacity is. On EKS: an IAM role `zoorik-agent-<cluster>` whose
  policy is limited to this cluster's node groups, plus reading the region's spot vCPU quota and how
  likely spot capacity is, bound with EKS Pod Identity.
- **A VM's data disks** (`vm connect`, if you agree). On Azure: Zoorik's app joins your Microsoft Entra
  directory, the first time only, with the custom role `Zoorik Amoeba VM Disks v1 <subscription>` on
  the VM's resource group (and on the disk encryption sets its data disks use in other groups) and the
  read-only role `Zoorik Amoeba Catalog Reader v1 <subscription>` on the subscription. The tag
  `zoorik-amoeba-org` on the resource group binds the group to your organisation. On AWS: the IAM role
  `ZoorikAmoeba-<instance-id>`, which Zoorik may assume only with your organisation's ExternalId and
  which may change only the volumes and snapshots tagged `zoorik-amoeba-vm=<instance-id>`, and that
  tag on the instance's data volumes. If `az` or `aws` is missing or not signed in, or you say no, the
  VM still connects, and `vm connect` prints the same changes as commands to run in Azure Cloud Shell
  or AWS CloudShell.

Zoorik never creates a machine or raises a node pool's size. Before the agent lowers a minimum, it
records yours in a tag, and `disconnect` puts it back, which may add machines up to it.

### On disconnect

`disconnect` needs only your credentials and works whether or not the agent still runs. It reads
everything first, and asks one question only when `zoorikctl` left something in the cloud. It stops
the agent and tells Zoorik that the cluster leaves the console. On a yes, it puts back the node pool
minimums Zoorik lowered and removes the access `zoorikctl` granted in the cloud. It removes the
webhook, roles and policies, and hands back what Zoorik changed on your autoscalers, workloads and
nodes, each field only if it is still as Zoorik left it; on EKS it removes the admission policy and
scales the cluster autoscaler back to what it was. On AKS a cluster autoscaler that `prepare` turned
off stays off, because Node Auto Provisioning stays on. It moves your apps off Zoorik's own
NodePools onto your machines, then empties and deletes those pools; a pod that cannot move holds its
pool, and the run names it. Last, it deletes the resource definitions, the Helm release and the
namespace. A disconnect that stops part-way picks up where it left off when you run it again.

## Build from source

You need Go (the version in [`go.mod`](go.mod)).

```sh
git clone https://github.com/getzoorik/zoorikctl.git
cd zoorikctl
go build -o bin/zoorikctl ./cmd/zoorikctl
./bin/zoorikctl version
```

To stamp a version, add `-ldflags "-X github.com/getzoorik/zoorikctl/internal/api.version=v0.0.0-dev"`.

## Contributing

Issues and pull requests are welcome. Our [contributing guide](https://github.com/getzoorik/.github/blob/main/CONTRIBUTING.md)
says how. Before you open a pull request, run the checks CI runs:

```sh
go mod tidy && git diff --exit-code go.mod go.sum
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/zoorikctl
done
```

The repository is laid out like this:

- `cmd/zoorikctl` and `internal/`: the CLI;
- `install/`: the installers. Change them here;
- `macos`, `linux` and `windows.ps1`: the copies get.zoorik.com serves, with `CNAME` and `.nojekyll`.
  Each release copies `install/` over them, so please do not edit them by hand;
- `packaging/`: the Homebrew formula and Scoop manifest each release renders into
  [getzoorik/homebrew-tap](https://github.com/getzoorik/homebrew-tap) and
  [getzoorik/scoop-bucket](https://github.com/getzoorik/scoop-bucket);
- `.krew.yaml`: the [Krew](https://krew.sigs.k8s.io) plugin manifest for `kubectl zoorik`, rendered
  for each release into [kubernetes-sigs/krew-index](https://github.com/kubernetes-sigs/krew-index).

## Security

Please report a vulnerability privately: use **Report a vulnerability** on this repository's
**Security** tab, or write to [founders@zoorik.com](mailto:founders@zoorik.com). Please do not open a
public issue. Our [security policy](https://github.com/getzoorik/.github/blob/main/SECURITY.md) has the
details.

## License

Apache-2.0, see [LICENSE](LICENSE). Copyright 2026 Zoorik Private Limited.

---

<p align="center"><sub><a href="https://zoorik.com">zoorik.com</a> · <a href="https://zoorik.com/docs">Docs</a> · <a href="https://zoorik.com/security">Security</a> · <a href="https://github.com/getzoorik">Zoorik on GitHub</a></sub></p>
<p align="center"><sub>The Zoorik, Coral and Amoeba names and logos belong to Zoorik Private Limited.</sub></p>
