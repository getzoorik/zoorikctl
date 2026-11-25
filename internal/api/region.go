package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/refusal"
)

const AmoebaAgentPath = "/api/agent"

type region struct {
	api     string
	console string
	amoeba  string
}

var regions = map[string]region{
	"us": {api: "https://dev.zoorik.com/coral", console: "https://dev.zoorik.com/coral",
		amoeba: "https://app.zoorik.com/amoeba"},
	"lab": {api: "https://lab.zoorik.com/coral", console: "https://lab.zoorik.com/coral",
		amoeba: "https://lab.zoorik.com/amoeba"},
}

func ConsoleURL(name, override string) string {
	if strings.TrimSpace(override) != "" {
		return ""
	}
	return regions[strings.ToLower(strings.TrimSpace(name))].console
}

func ResolveBaseURL(name, override string) (string, error) {
	if override != "" {
		return strings.TrimRight(override, "/"), nil
	}
	served, err := lookup(name)
	if err != nil {
		return "", err
	}
	return served.api, nil
}

func ResolveAmoebaURL(name, override string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		return strings.TrimRight(override, "/"), nil
	}
	served, err := lookup(name)
	if err != nil {
		return "", err
	}
	return served.amoeba, nil
}

func lookup(name string) (region, error) {
	if name == "" {
		return region{}, refusal.New(refusal.CodeUsage,
			"No API region was given, so there is no control plane to ask what to install.",
			fmt.Sprintf("Pass --api-region with one of: %s. The connect dialog prints the right "+
				"one on the command it generates.", strings.Join(known(), ", ")))
	}
	served, ok := regions[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return region{}, refusal.New(refusal.CodeUsage,
			fmt.Sprintf("%q is not a region this build serves.", name),
			fmt.Sprintf("Use one of: %s -- or pass --api-url to name a control plane directly.",
				strings.Join(known(), ", ")))
	}
	return served, nil
}

func known() []string {
	names := make([]string, 0, len(regions))
	for name := range regions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
