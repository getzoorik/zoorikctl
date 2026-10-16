package cloud

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/text"
)

type aksAgentPool struct {
	Name              string            `json:"name"`
	EnableAutoScaling bool              `json:"enableAutoScaling"`
	MinCount          *int              `json:"minCount"`
	MaxCount          *int              `json:"maxCount"`
	Tags              map[string]string `json:"tags"`
}

func (p aksAgentPool) restore() (minimumRestore, string, bool) {
	if !p.EnableAutoScaling {
		return restoreOf(p.Tags, aksOriginalMinimumTag, aksLoweredMinimumTag, nil, nil)
	}
	return restoreOf(p.Tags, aksOriginalMinimumTag, aksLoweredMinimumTag, p.MinCount, p.MaxCount)
}

type managedNodegroup struct {
	Name          string `json:"nodegroupName"`
	Arn           string `json:"nodegroupArn"`
	ScalingConfig struct {
		MinSize     *int `json:"minSize"`
		MaxSize     *int `json:"maxSize"`
		DesiredSize *int `json:"desiredSize"`
	} `json:"scalingConfig"`
	Tags map[string]string `json:"tags"`
}

func (g managedNodegroup) restore() (minimumRestore, string, bool) {
	return restoreOf(g.Tags, originalMinimumTag, loweredMinimumTag, g.ScalingConfig.MinSize,
		g.ScalingConfig.MaxSize)
}

type autoScalingGroup struct {
	Name    string `json:"AutoScalingGroupName"`
	MinSize int    `json:"MinSize"`
	MaxSize int    `json:"MaxSize"`
	Tags    []struct {
		Key   string `json:"Key"`
		Value string `json:"Value"`
	} `json:"Tags"`
}

func (g autoScalingGroup) tags() map[string]string {
	tags := make(map[string]string, len(g.Tags))
	for _, tag := range g.Tags {
		tags[tag.Key] = tag.Value
	}
	return tags
}

func (g autoScalingGroup) restore() (minimumRestore, string, bool) {
	return restoreOf(g.tags(), originalMinimumTag, loweredMinimumTag, &g.MinSize, &g.MaxSize)
}

func (g autoScalingGroup) ofCluster(cluster string) bool {
	tags := g.tags()
	if _, managed := tags["eks:nodegroup-name"]; managed {
		return false
	}
	return tags["kubernetes.io/cluster/"+cluster] == "owned" || tags["eks:cluster-name"] == cluster
}

type minimumRestore struct {
	original int
	lowered  int
	current  int
	maximum  int
	target   int
	sized    bool
	raise    bool
	changed  bool
	untagged bool
}

func restoreOf(tags map[string]string, key, loweredKey string, minimum, maximum *int) (minimumRestore, string,
	bool) {
	recorded, tagged := tagIn(tags, key)
	if !tagged {
		return minimumRestore{}, "", true
	}
	original, err := strconv.Atoi(strings.TrimSpace(recorded))
	if err != nil || original < 0 {
		return minimumRestore{}, recorded, false
	}
	restore := minimumRestore{original: original}
	if minimum == nil || maximum == nil {
		return restore, recorded, true
	}
	restore.sized = true
	restore.current, restore.maximum = *minimum, *maximum
	restore.target = min(original, *maximum)
	if restore.target <= *minimum {
		return restore, recorded, true
	}
	set, setTagged := tagIn(tags, loweredKey)
	lowered, err := strconv.Atoi(strings.TrimSpace(set))
	switch {
	case !setTagged || err != nil:
		restore.untagged = true
	case lowered != *minimum:
		restore.lowered, restore.changed = lowered, true
	default:
		restore.raise = true
	}
	return restore, recorded, true
}

func (r minimumRestore) title(kind, name string) string {
	if r.raise {
		return fmt.Sprintf("Put the minimum of %s %s back to %d (now %d)", kind, name, r.target, r.current)
	}
	return "Remove Zoorik's minimum tags from " + kind + " " + name
}

func (r minimumRestore) why(tags, raised string) string {
	switch {
	case !r.sized:
		return "It has no minimum now, so there is nothing to put back; only the tags " + tags + ", which " +
			"recorded the minimum before Zoorik lowered it, are removed."
	case r.changed:
		return fmt.Sprintf("Zoorik set its minimum to %d, and it is %d now, so someone changed it since; it is "+
			"left as it is, and only the tags %s are removed.", r.lowered, r.current, tags)
	case r.untagged:
		return fmt.Sprintf("It carries no readable tag of the minimum Zoorik set, so zoorikctl cannot tell "+
			"Zoorik's %d from one you set; it is left as it is, and only the tags %s are removed. If Zoorik "+
			"lowered it, put it back to %d yourself.", r.current, tags, r.target)
	case !r.raise:
		return fmt.Sprintf("Its minimum is %d, already at or above the %d it goes back to, so only the tags %s, "+
			"which recorded the minimum before Zoorik lowered it, are removed.", r.current, r.target, tags)
	}
	why := fmt.Sprintf("Zoorik lowered it from %d as it removed nodes. %s", r.original, raised)
	if r.target < r.original {
		why += fmt.Sprintf(" Its maximum is now %d, so the minimum goes back to %d, not %d.", r.maximum, r.target,
			r.original)
	}
	return why + " The tags " + tags + " are removed afterwards."
}

func (r minimumRestore) found(kind, name string) string {
	if !r.sized {
		return fmt.Sprintf("The %s %s had the minimum %d before Zoorik lowered it, and has no minimum now", kind,
			name, r.original)
	}
	return fmt.Sprintf("The %s %s had the minimum %d before Zoorik lowered it, and has %d now", kind, name,
		r.original, r.current)
}

func planMinimumRestoreAKS(ctx context.Context, runner *Runner, plan *Plan, cluster AzureCluster) {
	var pools []aksAgentPool
	if err := runner.JSON(ctx, Cmd(azureCLI, "aks", "nodepool", "list", "--subscription", cluster.Subscription,
		"--resource-group", cluster.ResourceGroup, "--cluster-name", cluster.Name, "-o", "json"), &pools); err != nil {
		plan.note(unlistedMinimums("Azure would not list the agent pools of "+cluster.ID()+" ("+text.OneLine(
			err.Error())+")", "agent pool", aksOriginalMinimumTag, aksLoweredMinimumTag))
		return
	}
	for _, pool := range pools {
		if _, tagged := tagIn(pool.Tags, aksOriginalMinimumTag); !tagged {
			continue
		}
		restore, recorded, ok := pool.restore()
		if !ok {
			plan.note(unreadMinimum("Agent pool "+pool.Name, aksOriginalMinimumTag, recorded))
			continue
		}
		plan.Found = append(plan.Found, restore.found("agent pool", pool.Name))
		plan.Steps = append(plan.Steps, aksMinimumRestoreStep(runner, cluster, pool, restore))
	}
}

func aksMinimumRestoreStep(runner *Runner, cluster AzureCluster, pool aksAgentPool, restore minimumRestore) Step {
	show := Cmd(azureCLI, "aks", "nodepool", "show", "--subscription", cluster.Subscription, "--resource-group",
		cluster.ResourceGroup, "--cluster-name", cluster.Name, "--name", pool.Name, "-o", "json")
	current := func(ctx context.Context) (aksAgentPool, bool, error) {
		var now aksAgentPool
		err := runner.JSON(ctx, show, &now)
		if NotFound(err) {
			return aksAgentPool{}, false, nil
		}
		return now, err == nil && carriesMinimumTags(now.Tags, aksOriginalMinimumTag, aksLoweredMinimumTag), err
	}
	return Step{
		RestoresMinimum: true,
		Title:           restore.title("agent pool", pool.Name),
		Why: restore.why(aksOriginalMinimumTag+" and "+aksLoweredMinimumTag, fmt.Sprintf("Raising it back lets "+
			"the cluster autoscaler add machines up to %d again.", restore.target)),
		Commands: []string{aksRestoreCommand(cluster, pool, restore).String()},
		Check: func(ctx context.Context) (bool, error) {
			_, tagged, err := current(ctx)
			return err == nil && !tagged, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			now, tagged, err := current(ctx)
			if err != nil || !tagged {
				return err
			}
			fresh, recorded, ok := now.restore()
			if !ok {
				return fmt.Errorf("%s", unreadMinimum("agent pool "+pool.Name, aksOriginalMinimumTag, recorded))
			}
			return runner.Run(ctx, aksRestoreCommand(cluster, now, fresh))
		},
	}
}

func aksRestoreCommand(cluster AzureCluster, pool aksAgentPool, restore minimumRestore) Command {
	args := []string{"aks", "nodepool", "update", "--subscription", cluster.Subscription, "--resource-group",
		cluster.ResourceGroup, "--cluster-name", cluster.Name, "--name", pool.Name}
	if restore.raise {
		args = append(args, "--update-cluster-autoscaler", "--min-count", strconv.Itoa(restore.target),
			"--max-count", strconv.Itoa(restore.maximum))
	}
	kept := otherTags(pool.Tags, aksOriginalMinimumTag, aksLoweredMinimumTag)
	if len(kept) == 0 {
		kept = []string{""}
	}
	args = append(append(args, "--tags"), kept...)
	return Cmd(azureCLI, append(args, "-o", "none")...)
}

func planMinimumRestoreEKS(ctx context.Context, runner *Runner, plan *Plan, cluster EKSCluster) {
	var listed struct {
		Nodegroups []string `json:"nodegroups"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "list-nodegroups", "--cluster-name",
		cluster.Name)...), &listed); err != nil {
		plan.note(unlistedMinimums("AWS would not list the node groups of "+cluster.ARN()+" ("+text.OneLine(
			err.Error())+")", "node group", originalMinimumTag, loweredMinimumTag))
	}
	for _, name := range listed.Nodegroups {
		group, exists, err := describeNodegroup(ctx, runner, cluster, name)
		if err != nil {
			plan.note(unlistedMinimums("AWS would not describe the node group "+name+" ("+text.OneLine(
				err.Error())+")", "node group", originalMinimumTag, loweredMinimumTag))
			continue
		}
		if _, tagged := tagIn(group.Tags, originalMinimumTag); !exists || !tagged {
			continue
		}
		restore, recorded, ok := group.restore()
		if !ok {
			plan.note(unreadMinimum("Node group "+name, originalMinimumTag, recorded))
			continue
		}
		plan.Found = append(plan.Found, restore.found("node group", name))
		plan.Steps = append(plan.Steps, nodegroupRestoreStep(runner, cluster, group, restore))
	}

	var described struct {
		AutoScalingGroups []autoScalingGroup `json:"AutoScalingGroups"`
	}
	if err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "autoscaling", "describe-auto-scaling-groups",
		"--filters", "Name=tag-key,Values="+originalMinimumTag)...), &described); err != nil {
		plan.note(unlistedMinimums("AWS would not list the Auto Scaling groups tagged "+originalMinimumTag+" ("+
			text.OneLine(err.Error())+")", "Auto Scaling group", originalMinimumTag, loweredMinimumTag))
	}
	for _, group := range described.AutoScalingGroups {
		if !group.ofCluster(cluster.Name) {
			continue
		}
		restore, recorded, ok := group.restore()
		if !ok {
			plan.note(unreadMinimum("Auto Scaling group "+group.Name, originalMinimumTag, recorded))
			continue
		}
		plan.Found = append(plan.Found, restore.found("Auto Scaling group", group.Name))
		plan.Steps = append(plan.Steps, autoScalingGroupRestoreStep(runner, cluster, group, restore))
	}
}

func describeNodegroup(ctx context.Context, runner *Runner, cluster EKSCluster, name string) (managedNodegroup,
	bool, error) {
	var described struct {
		Nodegroup managedNodegroup `json:"nodegroup"`
	}
	err := runner.JSON(ctx, Cmd(awsCLI, awsArgs(cluster, "eks", "describe-nodegroup", "--cluster-name", cluster.Name,
		"--nodegroup-name", name)...), &described)
	if NotFound(err) {
		return managedNodegroup{}, false, nil
	}
	return described.Nodegroup, err == nil, err
}

func nodegroupRestoreStep(runner *Runner, cluster EKSCluster, group managedNodegroup, restore minimumRestore) Step {
	current := func(ctx context.Context) (managedNodegroup, bool, error) {
		now, exists, err := describeNodegroup(ctx, runner, cluster, group.Name)
		return now, exists && carriesMinimumTags(now.Tags, originalMinimumTag, loweredMinimumTag), err
	}
	update := func(now managedNodegroup, restore minimumRestore) Command {
		desired := restore.target
		if now.ScalingConfig.DesiredSize != nil {
			desired = max(desired, *now.ScalingConfig.DesiredSize)
		}
		return Cmd(awsCLI, awsArgs(cluster, "eks", "update-nodegroup-config", "--cluster-name", cluster.Name,
			"--nodegroup-name", now.Name, "--scaling-config", fmt.Sprintf("minSize=%d,maxSize=%d,desiredSize=%d",
				restore.target, restore.maximum, desired))...)
	}
	untag := func(now managedNodegroup) Command {
		return Cmd(awsCLI, awsArgs(cluster, "eks", "untag-resource", "--resource-arn", now.Arn, "--tag-keys",
			originalMinimumTag, loweredMinimumTag)...)
	}
	var commands []string
	if restore.raise {
		commands = append(commands, update(group, restore).String())
	}
	commands = append(commands, untag(group).String())
	return Step{
		RestoresMinimum: true,
		Title:           restore.title("node group", group.Name),
		Why: restore.why(originalMinimumTag+" and "+loweredMinimumTag, fmt.Sprintf("Raising it back sets its "+
			"desired size to at least %d, so EKS may add machines to it.", restore.target)),
		Commands: commands,
		Check: func(ctx context.Context) (bool, error) {
			_, tagged, err := current(ctx)
			return err == nil && !tagged, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			now, tagged, err := current(ctx)
			if err != nil || !tagged {
				return err
			}
			fresh, recorded, ok := now.restore()
			if !ok {
				return fmt.Errorf("%s", unreadMinimum("node group "+now.Name, originalMinimumTag, recorded))
			}
			if fresh.raise {
				if err := runner.Run(ctx, update(now, fresh)); err != nil {
					return err
				}
				note(fmt.Sprintf("waiting for EKS to set the minimum of %s to %d", now.Name, fresh.target))
				applied, err := waitFor(ctx, func() (bool, error) {
					after, exists, err := describeNodegroup(ctx, runner, cluster, now.Name)
					if err != nil || !exists {
						return !exists, err
					}
					return after.ScalingConfig.MinSize != nil && *after.ScalingConfig.MinSize >= fresh.target, nil
				})
				if err != nil {
					return err
				}
				if !applied {
					return fmt.Errorf("EKS did not set the minimum of node group %s to %d within %s; its tags stay, "+
						"so running this disconnect again resumes here", now.Name, fresh.target, crdWait)
				}
			}
			return runner.Run(ctx, untag(now))
		},
	}
}

func autoScalingGroupRestoreStep(runner *Runner, cluster EKSCluster, group autoScalingGroup,
	restore minimumRestore) Step {
	describe := Cmd(awsCLI, awsArgs(cluster, "autoscaling", "describe-auto-scaling-groups",
		"--auto-scaling-group-names", group.Name)...)
	current := func(ctx context.Context) (autoScalingGroup, bool, error) {
		var described struct {
			AutoScalingGroups []autoScalingGroup `json:"AutoScalingGroups"`
		}
		if err := runner.JSON(ctx, describe, &described); err != nil {
			return autoScalingGroup{}, false, err
		}
		for _, now := range described.AutoScalingGroups {
			if now.Name == group.Name {
				return now, carriesMinimumTags(now.tags(), originalMinimumTag, loweredMinimumTag), nil
			}
		}
		return autoScalingGroup{}, false, nil
	}
	update := func(restore minimumRestore) Command {
		return Cmd(awsCLI, awsArgs(cluster, "autoscaling", "update-auto-scaling-group", "--auto-scaling-group-name",
			group.Name, "--min-size", strconv.Itoa(restore.target))...)
	}
	untagged := make([]map[string]any, 0, 2)
	for _, key := range []string{originalMinimumTag, loweredMinimumTag} {
		untagged = append(untagged, map[string]any{"ResourceId": group.Name, "ResourceType": "auto-scaling-group",
			"Key": key})
	}
	untag := Cmd(awsCLI, awsArgs(cluster, "autoscaling", "delete-tags", "--tags", compactJSON(untagged))...)
	var commands []string
	if restore.raise {
		commands = append(commands, update(restore).String())
	}
	commands = append(commands, untag.String())
	return Step{
		RestoresMinimum: true,
		Title:           restore.title("Auto Scaling group", group.Name),
		Why: restore.why(originalMinimumTag+" and "+loweredMinimumTag, fmt.Sprintf("Raising it back makes AWS "+
			"raise the group's desired capacity to at least %d, so it may add machines.", restore.target)),
		Commands: commands,
		Check: func(ctx context.Context) (bool, error) {
			_, tagged, err := current(ctx)
			return err == nil && !tagged, err
		},
		Apply: func(ctx context.Context, note func(string)) error {
			now, tagged, err := current(ctx)
			if err != nil || !tagged {
				return err
			}
			fresh, recorded, ok := now.restore()
			if !ok {
				return fmt.Errorf("%s", unreadMinimum("Auto Scaling group "+now.Name, originalMinimumTag, recorded))
			}
			if fresh.raise {
				if err := runner.Run(ctx, update(fresh)); err != nil {
					return err
				}
			}
			return runner.Run(ctx, untag)
		},
	}
}

func unlistedMinimums(why, kind, originalKey, loweredKey string) string {
	return why + ", so no " + kind + " minimum Zoorik lowered is put back. If Zoorik lowered any, set each " + kind +
		" tagged " + originalKey + " back to that value and remove the tags " + originalKey + " and " + loweredKey +
		" yourself."
}

func unreadMinimum(what, key, recorded string) string {
	return what + " carries " + key + "=" + recorded + ", which is not a whole number, so zoorikctl leaves its " +
		"minimum and tags as they are."
}

func carriesMinimumTags(tags map[string]string, keys ...string) bool {
	for _, key := range keys {
		if _, tagged := tagIn(tags, key); tagged {
			return true
		}
	}
	return false
}

func tagIn(tags map[string]string, key string) (string, bool) {
	for name, value := range tags {
		if strings.EqualFold(name, key) {
			return value, true
		}
	}
	return "", false
}

func otherTags(tags map[string]string, dropped ...string) []string {
	kept := make([]string, 0, len(tags))
	for name, value := range tags {
		if !carriesMinimumTags(map[string]string{name: value}, dropped...) {
			kept = append(kept, name+"="+value)
		}
	}
	sort.Strings(kept)
	return kept
}
