package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/refusal"
)

const (
	systemdRunDir = "/run/systemd/system"

	ebsLinkPrefix = "nvme-Amazon_Elastic_Block_Store_"

	ebsLinks = "/dev/disk/by-id/" + ebsLinkPrefix + "vol*"
)

func Preflight(ctx context.Context) (*Machine, error) {
	if runtime.GOOS != "linux" {
		return nil, notSupported("zoorikctl vm connect runs on the Linux VM it connects, and this machine runs "+
			platformName(runtime.GOOS)+".",
			"Sign in to the VM over SSH and run the two lines from the console's Connect a VM dialog there.")
	}
	if _, err := os.Stat(systemdRunDir); err != nil || !onPath("systemctl") {
		return nil, notSupported("This VM does not run systemd, and the Amoeba agent runs as a systemd service.", "")
	}
	machine, found := readAzure(ctx)
	if !found {
		machine, found = readAWS(ctx)
	}
	if !found {
		return nil, notSupported("This machine is not an Azure or AWS VM: neither cloud's metadata service "+
			"answered at 169.254.169.254.",
			"Amoeba connects Azure and AWS VMs. Run this on the VM itself.")
	}
	switch machine.Cloud {
	case kube.CloudAzure:
		if !strings.EqualFold(machine.OSType, "Linux") {
			return nil, notSupported("Azure reports this VM's OS as "+machine.OSType+", and zoorikctl vm connect "+
				"connects Linux VMs.", "")
		}
		if len(machine.DataDisks) == 0 {
			return nil, notSupported("This VM has no data disk. Amoeba rightsizes data disks; the OS disk stays as "+
				"it is.", "Attach a data disk to the VM, then run this again.")
		}
	case kube.CloudAWS:
		switch ebsVolumes() {
		case 0:
			return nil, notSupported("This instance's EBS volumes are not NVMe devices, so its instance type runs "+
				"on Xen. Amoeba needs an instance type built on the Nitro System, whose EBS volumes appear as "+
				"NVMe devices.", "Change the instance to a Nitro instance type, then run this again.")
		case 1:
			return nil, notSupported("This instance has only its root volume. Amoeba rightsizes data volumes; the "+
				"root volume stays as it is.", "Attach a data volume to the instance, then run this again.")
		}
	}
	return machine, nil
}

func ebsVolumes() int {
	links, _ := filepath.Glob(ebsLinks)
	volumes := map[string]bool{}
	for _, link := range links {
		volume := strings.TrimPrefix(filepath.Base(link), ebsLinkPrefix)
		if cut := strings.IndexAny(volume, "-_"); cut >= 0 {
			volume = volume[:cut]
		}
		volumes[volume] = true
	}
	return len(volumes)
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func platformName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	}
	return goos
}

func notSupported(what, next string) error {
	return refusal.New(refusal.CodeNotSupported, what, next)
}
