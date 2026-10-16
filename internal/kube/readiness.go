package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const invalidImageName = "InvalidImageName"

var stallReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"CrashLoopBackOff":           true,
	invalidImageName:             true,
}

func WaitForAgent(ctx context.Context, target *Target, timeout time.Duration,
	progress func(string)) error {
	deadline := time.Now().Add(timeout)
	var lastReason string

	for {
		pods, err := target.Clientset.CoreV1().Pods(AgentNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=" + AgentWorkload,
		})
		if err != nil && !apierrors.IsForbidden(err) {
			return refusal.Wrap(refusal.CodeAgentNotReady, err,
				"The install applied, but the agent's pods could not be read back.",
				"Check with `"+target.Kubectl("-n "+AgentNamespace+" get pods")+"` -- the objects "+
					"were created, so this is a read permission or a connectivity problem, not an "+
					"install one.")
		}
		if apierrors.IsForbidden(err) {
			return nil
		}

		var stalled string
		var stalledPod corev1.Pod
		for _, pod := range pods.Items {
			if isRunningAndReady(pod) {
				return nil
			}
			reason, stall := waitingReason(pod)
			switch {
			case stall == invalidImageName:
				return imagePullOrCrash(target, reason, pod)
			case stall != "":
				stalled, stalledPod = reason, pod
			}
			if reason != "" && reason != lastReason {
				lastReason = reason
				progress(reason)
			}
		}

		if len(pods.Items) == 0 && lastReason != "no pod has been scheduled yet" {
			lastReason = "no pod has been scheduled yet"
			progress(lastReason)
		}

		if time.Now().After(deadline) {
			if stalled != "" {
				return imagePullOrCrash(target, stalled, stalledPod)
			}
			return refusal.New(refusal.CodeAgentNotReady,
				fmt.Sprintf("The install applied, but no agent pod became ready within %s.",
					timeout),
				"Everything was created, so this is safe to leave running and check again. "+
					"`"+target.Kubectl("-n "+AgentNamespace+" describe pod")+"` says what the pod "+
					"is waiting for -- most often a node with room for a 100m/256Mi request.").
				WithDetail("Last state seen: " + text.OrNone(lastReason))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func isRunningAndReady(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func waitingReason(pod corev1.Pod) (string, string) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting == nil {
			continue
		}
		reason := status.State.Waiting.Reason
		if stallReasons[reason] {
			return reason + ": " + status.State.Waiting.Message, reason
		}
		return reason, ""
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return "not scheduled: " + condition.Message, ""
		}
	}
	return string(pod.Status.Phase), ""
}

func imagePullOrCrash(target *Target, reason string, pod corev1.Pod) error {
	image := ""
	if len(pod.Spec.Containers) > 0 {
		image = pod.Spec.Containers[0].Image
	}

	if strings.HasPrefix(reason, "ImagePullBackOff") || strings.HasPrefix(reason, "ErrImagePull") ||
		strings.HasPrefix(reason, "InvalidImageName") {
		if !namesRegistry(image) {
			return refusal.New(refusal.CodeAgentNotReady,
				"Everything was created, but the node the agent landed on does not hold the "+
					"agent image, and the image names no registry to pull it from.",
				"An image with no registry host is a locally built one. The control plane pins "+
					"the tag it was given in "+AgentImageTagSetting+"; build the agent image under "+
					"exactly that name, make it visible to this cluster's nodes (for kind, `"+
					kindLoad(target, image)+"`), and the kubelet starts the pod on its own. "+
					"Nothing needs to be re-applied.").
				WithDetail(
					"Image:  "+text.OrNone(image),
					"Reason: "+reason)
		}
		return refusal.New(refusal.CodeAgentNotReady,
			"Everything was created, but this cluster cannot pull the agent image.",
			"The image is public and needs no credentials, so this is almost always egress: ask "+
				"whoever runs the cluster to allow the registry host above, or to mirror that "+
				"image. Nothing needs to be re-applied once they have -- the kubelet keeps "+
				"retrying and the pod will start on its own.").
			WithDetail(
				"Image:  "+text.OrNone(image),
				"Reason: "+reason)
	}

	if strings.HasPrefix(reason, "CreateContainerConfigError") {
		return refusal.New(refusal.CodeAgentNotReady,
			"Everything was created, but the agent container could not be configured.",
			"This means an object the pod mounts is missing -- normally the bootstrap Secret or "+
				"the config map. Re-run this exact command: it applies both by name and will "+
				"replace whatever is incomplete.").
			WithDetail("Reason: " + reason)
	}

	return refusal.New(refusal.CodeAgentNotReady,
		"Everything was created, but the agent container is restarting.",
		"`"+target.Kubectl("-n "+AgentNamespace+" logs deploy/"+AgentWorkload)+"` has the "+
			"reason. The install itself is complete, so nothing needs re-applying.").
		WithDetail("Reason: " + reason)
}

func namesRegistry(image string) bool {
	slash := strings.Index(image, "/")
	if slash < 0 {
		return false
	}
	host := image[:slash]
	return strings.ContainsAny(host, ".:") || host == "localhost"
}

func kindLoad(target *Target, image string) string {
	command := "kind load docker-image " + ShellWord(image)
	if name, ok := strings.CutPrefix(target.ContextName, KindContextPrefix); ok && name != "" {
		command += " --name " + ShellWord(name)
	}
	return command
}
