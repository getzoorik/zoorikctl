package kube

import (
	"strings"

	"github.com/getzoorik/zoorikctl/internal/setup"
)

const (
	ReleasePolicy = "zoorik-agent-release"

	ReleaseImagesPolicy = "zoorik-agent-release-images"

	ReleaseParamsConfigMap = "zoorik-agent-release-params"

	AgentRegistryKey = "agentRegistry"

	NodeRegistryKey = "nodeRegistry"

	agentRepositoryPath = "/coral/agent"

	nodeRepositoryPath = "/coral/node-daemon"
)

func AgentRegistry(imageRef string) string {
	return registryOf(imageRef, agentRepositoryPath)
}

func NodeRegistry(docs []setup.Document) string {
	for _, image := range setup.Summarize(docs).Images {
		if registry := registryOf(image, nodeRepositoryPath); registry != "" {
			return registry
		}
	}
	return ""
}

func registryOf(imageRef, repositoryPath string) string {
	repository := strings.TrimSpace(imageRef)
	if at := strings.IndexByte(repository, '@'); at >= 0 {
		repository = repository[:at]
	}
	if colon := strings.LastIndexByte(repository, ':'); colon > strings.LastIndexByte(repository, '/') {
		repository = repository[:colon]
	}
	registry, found := strings.CutSuffix(repository, repositoryPath)
	if !found {
		return ""
	}
	return registry
}
