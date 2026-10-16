package kube

import (
	"regexp"
	"strings"
)

type ClusterResource struct {
	Cloud   string
	Account string
	Region  string
	Name    string
	Raw     string
}

var (
	armManagedCluster = regexp.MustCompile(`^/subscriptions/` +
		`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/resourcegroups/([^/?#\x00-\x20]+)` +
		`/providers/microsoft\.containerservice/managedclusters/([^/?#\x00-\x20]+)$`)
	eksClusterARN             = regexp.MustCompile(`^arn:(aws(?:-[a-z]+)*):eks:([a-z0-9-]+):([0-9]{12}):cluster/([^/:?#\x00-\x20]+)$`)
	azureProviderSubscription = regexp.MustCompile(`(?i)^azure:///subscriptions/([^/]+)/`)
)

func ParseClusterResource(raw string) (ClusterResource, bool) {
	pin := strings.TrimSpace(raw)
	folded := asciiLower(pin)
	if at := armManagedCluster.FindStringSubmatchIndex(folded); at != nil {
		return ClusterResource{Cloud: CloudAzure, Account: folded[at[2]:at[3]],
			Name: pin[at[6]:at[7]], Raw: pin}, true
	}
	if at := eksClusterARN.FindStringSubmatchIndex(folded); at != nil {
		return ClusterResource{Cloud: CloudAWS, Account: folded[at[6]:at[7]],
			Region: folded[at[4]:at[5]], Name: pin[at[8]:at[9]], Raw: pin}, true
	}
	return ClusterResource{}, false
}

func asciiLower(value string) string {
	lowered := []byte(value)
	for i, b := range lowered {
		if 'A' <= b && b <= 'Z' {
			lowered[i] = b + ('a' - 'A')
		}
	}
	return string(lowered)
}

func azureSubscriptionOf(providerID string) string {
	match := azureProviderSubscription.FindStringSubmatch(strings.TrimSpace(providerID))
	if match == nil {
		return ""
	}
	return strings.ToLower(match[1])
}
