package privileged

import (
	"regexp"
	"slices"
	"strings"
)

// The COSI resource types omnictl and talosctl may name: an allowlist, so a type a release adds,
// or one the executor's authors did not think of, is refused until it is read and added here. Each
// holds state an operator reads to see how a cluster is doing, and none holds a credential. Left
// out on purpose, among others: Omni's ConfigPatches, Configs, cluster and machine configs (even
// redacted, and MachinePendingUpdates's config diff), join tokens, kubeconfigs, talosconfigs and secrets; MachineStatuses, KernelArgs and
// KernelArgsStatuses, which hold a machine's kernel command line, where Omni's installation media
// put the SideroLink join token (siderolink.api=...?jointoken=); Talos's machine config, LinkSpecs,
// RegistryConfigs, Manifests, KubeSpan and discovery configs, VolumeConfigs, extension service
// configs and the Siderolink config. The executor's proxy also asks the server for each type's
// resource definition and refuses one marked sensitive, and refuses the names secretType matches,
// so an entry here that is wrong still does not open a secret.
var (
	omniReadable = typeSet("omni.sidero.dev",
		"Clusters", "ClusterStatuses", "ClusterMachines", "ClusterMachineStatuses",
		"ClusterBootstrapStatuses", "ClusterDestroyStatuses", "ClusterDiagnostics", "ClusterKubernetesNodes",
		"ClusterMachineTalosVersions", "ClusterKubernetesManifestsStatuses",
		"ControlPlaneStatuses", "KubernetesStatuses", "KubernetesUpgradeStatuses", "KubernetesUpgradeManifestStatuses",
		"KubernetesVersions", "TalosVersions", "TalosUpgradeStatuses", "MachineSets", "MachineSetStatuses", "MachineSetNodes",
		"MachineSetDestroyStatuses", "Machines", "MachineLabels", "MachineClasses", "MachineExtensions",
		"MachineExtensionsStatuses", "MachineUpgradeStatuses", "EtcdBackups", "EtcdBackupStatuses",
		"EtcdBackupOverallStatuses", "ExposedServices", "OngoingTasks", "Notifications",
		"LoadBalancerStatuses", "ImagePullStatuses",
	)
	talosReadable = slices.Concat(
		typeSet("cluster.talos.dev", "Members", "Affiliates", "Infos"),
		typeSet("net.talos.dev", "NodeAddresses", "AddressStatuses", "RouteStatuses", "LinkStatuses", "HostnameStatuses",
			"ResolverStatuses", "TimeServerStatuses", "NetworkStatuses", "HardwareAddresses", "ProbeStatuses"),
		typeSet("block.talos.dev", "Disks", "BlockDevices", "DiscoveredVolumes", "VolumeStatuses", "MountStatuses",
			"SystemDisks", "SwapStatuses", "ZswapStatuses"),
		typeSet("runtime.talos.dev", "MachineStatuses", "Versions", "ExtensionStatuses", "LoadedKernelModules", "KernelParamStatuses"),
		typeSet("v1alpha1.talos.dev", "Services", "TimeStatuses"),
		typeSet("config.talos.dev", "MachineTypes"),
		typeSet("etcd.talos.dev", "EtcdMembers"),
		typeSet("kubernetes.talos.dev", "NodeStatuses", "StaticPodStatuses", "ManifestStatuses", "Nodenames", "NodeIPs"),
		typeSet("perf.talos.dev", "CPUStats", "MemoryStats"),
		typeSet("hardware.talos.dev", "Processors", "MemoryModules", "SystemInformations", "PCIDevices"),
		typeSet("talos.dev", "SecurityStates"),
		typeSet("kubespan.talos.dev", "KubeSpanPeerStatuses"),
	)
	// metaTypes are COSI's own: the tools read them to find a type by its alias. They are read,
	// never written, and have no resource definition of their own to check.
	metaTypes = []string{"ResourceDefinitions.meta.cosi.dev", "Namespaces.meta.cosi.dev"}
	// omniSystemTypes are what omnictl reads before any command: Omni's version.
	omniSystemTypes = []string{"SysVersions.system.sidero.dev"}
)

func typeSet(suffix string, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n+"."+suffix)
	}
	return out
}

// sideroReadableTypes are the canonical types a tool's requests may name.
func sideroReadableTypes(tool string) []string {
	if tool == ToolTalosctl {
		return append(slices.Clone(talosReadable), metaTypes...)
	}
	return append(append(slices.Clone(omniReadable), metaTypes...), omniSystemTypes...)
}

// sideroReadable says whether a tool's request may name a type. COSI types are case-sensitive.
func sideroReadable(tool, typ string) bool {
	return slices.Contains(sideroReadableTypes(tool), typ)
}

// sideroTypeFor resolves a type as given on the command line: its canonical name, or its plural or
// singular name in lower case (clusters, cluster, members). The tools also accept abbreviations
// (mc, cm), which the executor refuses: the card names the type in full.
func sideroTypeFor(tool, word string) (string, bool) {
	w := strings.ToLower(word)
	for _, t := range sideroReadableTypes(tool) {
		if slices.Contains(metaTypes, t) || slices.Contains(omniSystemTypes, t) {
			continue
		}
		plural, _, _ := strings.Cut(t, ".")
		if w == strings.ToLower(t) || w == strings.ToLower(plural) || w == strings.ToLower(singular(plural)) {
			return t, true
		}
	}
	return "", false
}

// singular is a type's singular name, as COSI derives it for the usual plurals.
func singular(plural string) string {
	switch {
	case strings.HasSuffix(plural, "ies"):
		return strings.TrimSuffix(plural, "ies") + "y"
	case strings.HasSuffix(plural, "sses"), strings.HasSuffix(plural, "uses"), strings.HasSuffix(plural, "xes"):
		return strings.TrimSuffix(plural, "es")
	}
	return strings.TrimSuffix(plural, "s")
}

// typeNames lists the plural names of a tool's readable types, for a refusal's hint.
func typeNames(tool string) string {
	var out []string
	for _, t := range sideroReadableTypes(tool) {
		if slices.Contains(metaTypes, t) || slices.Contains(omniSystemTypes, t) {
			continue
		}
		plural, _, _ := strings.Cut(t, ".")
		out = append(out, strings.ToLower(plural))
	}
	slices.Sort(out)
	return strings.Join(slices.Compact(out), ", ")
}

// omniClusterLabel is the label Omni puts on a resource that belongs to a cluster.
const omniClusterLabel = "omni.sidero.dev/cluster"

// omniClusterIDTypes are Omni's types whose ID is their cluster's name.
var omniClusterIDTypes = typeSet("omni.sidero.dev",
	"Clusters", "ClusterStatuses", "ClusterBootstrapStatuses", "ClusterDestroyStatuses", "ClusterDiagnostics",
	"ClusterKubernetesManifestsStatuses", "KubernetesStatuses", "KubernetesUpgradeStatuses",
	"KubernetesUpgradeManifestStatuses", "TalosUpgradeStatuses", "LoadBalancerStatuses", "EtcdBackupStatuses",
)

// omniClusterOf says which cluster an Omni resource belongs to: its cluster label, or for a type
// named by its cluster, its ID. ok is false for a resource of no cluster (a machine not in one,
// a machine class).
func omniClusterOf(typ, id string, labels map[string]string) (cluster string, ok bool) {
	if c, ok := labels[omniClusterLabel]; ok {
		return c, true
	}
	if slices.Contains(omniClusterIDTypes, typ) {
		return id, true
	}
	return "", false
}

// talosImageRE is an installer image from Sidero's registries: the Image Factory or ghcr.io's
// siderolabs organisation. talosctl upgrade installs it as the node's operating system.
var talosImageRE = regexp.MustCompile(`^(factory\.talos\.dev|ghcr\.io/siderolabs)/[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[0-9a-f]{64})?$`)

func talosImage(s string) bool { return talosImageRE.MatchString(s) && !strings.Contains(s, "..") }
