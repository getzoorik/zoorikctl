package kube

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/setup"
	"github.com/getzoorik/zoorikctl/internal/text"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	WebhookWorkload = "zoorik-agent-webhook"

	WebhookService = "zoorik-agent-webhook"

	WebhookConfiguration = "zoorik-agent-webhook"

	WebhookProbeName = "probe.webhook.coral.zoorik.com"

	WebhookProbeLabel = "coral.zoorik.com/webhook-probe"

	WebhookProbeAnsweredBy = "coral.zoorik.com/webhook-probe-answered-by"

	WebhookPortSetting = "CORAL_WEBHOOK_PORT"

	WebhookPortName = "https"

	WebhookHostNetworkPort = 10263

	WebhookHostNetworkFlag = "--webhook-host-network"

	PodSecurityEnforceLabel = "pod-security.kubernetes.io/enforce"

	PodSecurityPrivileged = "privileged"

	webhookProbeNamePrefix = "zoorik-webhook-probe-"

	kindNamespace = "Namespace"

	kindService = "Service"

	defaultWebhookTimeout = 5 * time.Second

	timeoutMargin = 500 * time.Millisecond
)

type WebhookState struct {
	Ready  bool
	Pod    string
	Reason string
}

type ProbeResult struct {
	AnsweredBy string
	Elapsed    time.Duration
	Timeout    time.Duration
	TimedOut   bool
	Cause      string
}

func (r ProbeResult) Answered() bool {
	return r.AnsweredBy != ""
}

func SetupHasWebhook(docs []setup.Document) bool {
	for _, doc := range docs {
		if doc.GetKind() == kindMutatingWebhookConfiguration && doc.GetName() == WebhookConfiguration {
			return true
		}
	}
	return false
}

func RenderWebhookHostNetwork(docs []setup.Document) ([]string, error) {
	var changes []string
	deployment, service := false, false
	for _, object := range docs {
		switch {
		case object.GetKind() == kindDeployment && object.GetName() == WebhookWorkload:
			if err := hostNetworkDeployment(object); err != nil {
				return nil, err
			}
			deployment = true
			changes = append(changes, fmt.Sprintf("Deployment/%s runs on the host network (hostNetwork, "+
				"dnsPolicy ClusterFirstWithHostNet), listens on port %d and rolls one pod at a time "+
				"without a surge, because two of its pods cannot share that port on one node",
				WebhookWorkload, WebhookHostNetworkPort))
		case object.GetKind() == kindService && object.GetName() == WebhookService:
			if err := hostNetworkService(object); err != nil {
				return nil, err
			}
			service = true
			changes = append(changes, fmt.Sprintf("Service/%s sends to port %d", WebhookService,
				WebhookHostNetworkPort))
		case object.GetKind() == kindNamespace && object.GetName() == AgentNamespace:
			labels := object.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[PodSecurityEnforceLabel] = PodSecurityPrivileged
			object.SetLabels(labels)
			changes = append(changes, fmt.Sprintf("Namespace/%s enforces the %q Pod Security level, "+
				"because every stricter level refuses a pod on the host network", AgentNamespace,
				PodSecurityPrivileged))
		}
	}
	if !deployment || !service {
		return nil, refusal.New(refusal.CodeUsage,
			WebhookHostNetworkFlag+" moves the pod webhook onto the host network, but this control "+
				"plane's agent setup carries no pod webhook to move.",
			"Run it again without "+WebhookHostNetworkFlag+". Nothing has been applied.")
	}
	return changes, nil
}

func hostNetworkDeployment(object *unstructured.Unstructured) error {
	if err := unstructured.SetNestedField(object.Object, int64(0),
		"spec", "strategy", "rollingUpdate", "maxSurge"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(object.Object, int64(1),
		"spec", "strategy", "rollingUpdate", "maxUnavailable"); err != nil {
		return err
	}
	spec := []string{"spec", "template", "spec"}
	if err := unstructured.SetNestedField(object.Object, true, append(spec, "hostNetwork")...); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(object.Object, "ClusterFirstWithHostNet",
		append(spec, "dnsPolicy")...); err != nil {
		return err
	}
	containers, found, err := unstructured.NestedSlice(object.Object, append(spec, "containers")...)
	if err != nil || !found {
		return fmt.Errorf("Deployment/%s names no containers", WebhookWorkload)
	}
	for index, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		ports, _, _ := unstructured.NestedSlice(container, "ports")
		serving := false
		for at, raw := range ports {
			port, ok := raw.(map[string]any)
			if !ok || stringOf(port["name"]) != WebhookPortName {
				continue
			}
			port["containerPort"] = int64(WebhookHostNetworkPort)
			ports[at] = port
			serving = true
		}
		if !serving {
			continue
		}
		container["ports"] = ports
		container["env"] = withEnv(container["env"], WebhookPortSetting, strconv.Itoa(WebhookHostNetworkPort))
		containers[index] = container
	}
	return unstructured.SetNestedSlice(object.Object, containers, append(spec, "containers")...)
}

func withEnv(raw any, name, value string) []any {
	entries, _ := raw.([]any)
	for index, entry := range entries {
		variable, ok := entry.(map[string]any)
		if ok && stringOf(variable["name"]) == name {
			entries[index] = map[string]any{"name": name, "value": value}
			return entries
		}
	}
	return append(entries, map[string]any{"name": name, "value": value})
}

func hostNetworkService(object *unstructured.Unstructured) error {
	ports, found, err := unstructured.NestedSlice(object.Object, "spec", "ports")
	if err != nil || !found {
		return fmt.Errorf("Service/%s names no ports", WebhookService)
	}
	for index, raw := range ports {
		port, ok := raw.(map[string]any)
		if !ok || stringOf(port["name"]) != WebhookPortName {
			continue
		}
		port["targetPort"] = int64(WebhookHostNetworkPort)
		ports[index] = port
	}
	return unstructured.SetNestedSlice(object.Object, ports, "spec", "ports")
}

func WaitForWebhook(ctx context.Context, target *Target, timeout time.Duration,
	progress func(string)) (WebhookState, error) {
	deadline := time.Now().Add(timeout)
	var lastReason string
	selector := metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + WebhookWorkload}

	for {
		pods, err := target.Clientset.CoreV1().Pods(AgentNamespace).List(ctx, selector)
		if err != nil {
			if ctx.Err() != nil {
				return WebhookState{}, ctx.Err()
			}
			return WebhookState{Reason: "its pods could not be read: " + text.OneLine(err.Error())}, nil
		}
		for _, pod := range pods.Items {
			if isRunningAndReady(pod) {
				return WebhookState{Ready: true, Pod: pod.Name}, nil
			}
			reason, stall := waitingReason(pod)
			if stall == invalidImageName {
				return WebhookState{Pod: pod.Name, Reason: reason}, nil
			}
			if reason != "" && reason != lastReason {
				lastReason = reason
				progress("pod webhook: " + reason)
			}
		}
		if len(pods.Items) == 0 && lastReason != "no pod has been scheduled yet" {
			lastReason = "no pod has been scheduled yet"
			progress("pod webhook: " + lastReason)
		}
		if time.Now().After(deadline) {
			if failure := replicaFailure(ctx, target); failure != "" {
				return WebhookState{Reason: failure}, nil
			}
			return WebhookState{Reason: text.OrNone(lastReason)}, nil
		}
		select {
		case <-ctx.Done():
			return WebhookState{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func replicaFailure(ctx context.Context, target *Target) string {
	sets, err := target.Clientset.AppsV1().ReplicaSets(AgentNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + WebhookWorkload,
	})
	if err != nil {
		return ""
	}
	var newest metav1.Time
	message := ""
	for _, set := range sets.Items {
		for _, condition := range set.Status.Conditions {
			if condition.Type != "ReplicaFailure" || condition.Status != corev1.ConditionTrue {
				continue
			}
			if message == "" || newest.Before(&set.CreationTimestamp) {
				newest = set.CreationTimestamp
				message = "ReplicaSet " + set.Name + " FailedCreate: " + text.OneLine(condition.Message)
			}
		}
	}
	return message
}

func ProbeWebhook(ctx context.Context, target *Target) (ProbeResult, error) {
	configuration, err := target.Clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Get(ctx, WebhookConfiguration, metav1.GetOptions{})
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{Timeout: defaultWebhookTimeout}
	found := false
	for _, webhook := range configuration.Webhooks {
		if webhook.Name != WebhookProbeName {
			continue
		}
		found = true
		if webhook.TimeoutSeconds != nil {
			result.Timeout = time.Duration(*webhook.TimeoutSeconds) * time.Second
		}
		if len(webhook.ClientConfig.CABundle) == 0 {
			result.Cause = "the webhook has not published its certificate authority yet: the " +
				WebhookProbeName + " entry carries no caBundle"
			return result, nil
		}
	}
	if !found {
		result.Cause = "MutatingWebhookConfiguration " + WebhookConfiguration + " carries no " +
			WebhookProbeName + " entry"
		return result, nil
	}

	probe := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: webhookProbeNamePrefix,
			Namespace:    AgentNamespace,
			Labels:       map[string]string{WebhookProbeLabel: "true"},
		},
	}
	started := time.Now()
	answered, err := target.Clientset.CoreV1().ConfigMaps(AgentNamespace).Create(ctx, probe,
		metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: FieldManager})
	result.Elapsed = time.Since(started)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if apierrors.IsForbidden(err) {
			result.Cause = "the dry-run probe was refused: " + text.OneLine(err.Error())
			return result, nil
		}
		result.Cause = "the dry-run probe failed: " + text.OneLine(err.Error())
		return result, nil
	}
	if pod := strings.TrimSpace(answered.Annotations[WebhookProbeAnsweredBy]); pod != "" {
		result.AnsweredBy = pod
		return result, nil
	}
	if result.Elapsed+timeoutMargin >= result.Timeout {
		result.TimedOut = true
		result.Cause = fmt.Sprintf("the API server waited the webhook's whole %s timeout without an "+
			"answer, so it cannot reach the webhook pods over the network", result.Timeout)
		return result, nil
	}
	result.Cause = fmt.Sprintf("the API server admitted the probe after %s without the webhook "+
		"answering: the call failed at once (connection refused, or TLS not yet trusted)",
		result.Elapsed.Round(time.Millisecond))
	return result, nil
}
