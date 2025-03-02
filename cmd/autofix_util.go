package cmd

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"os"
	"strings"

	"github.com/Shopify/kubeaudit/scheme"
	"github.com/Shopify/yaml"
	log "github.com/sirupsen/logrus"
)

func getAuditFunctions() []interface{} {
	return []interface{}{
		auditAllowPrivilegeEscalation, auditReadOnlyRootFS, auditRunAsNonRoot,
		auditAutomountServiceAccountToken, auditPrivileged, auditCapabilities,
		auditAppArmor, auditSeccomp, auditNetworkPolicies, auditNamespaces,
	}
}

func fixPotentialSecurityIssue(resource Resource, result Result) Resource {
	resource = prepareResourceForFix(resource, result)

	for _, occurrence := range result.Occurrences {
		switch occurrence.id {
		case ErrorAllowPrivilegeEscalationNil, ErrorAllowPrivilegeEscalationTrue:
			resource = fixAllowPrivilegeEscalation(&result, resource, occurrence)
		case ErrorCapabilityNotDropped:
			resource = fixCapabilityNotDropped(&result, resource, occurrence)
		case ErrorCapabilityAdded:
			resource = fixCapabilityAdded(&result, resource, occurrence)
		case ErrorPrivilegedNil, ErrorPrivilegedTrue:
			resource = fixPrivileged(&result, resource, occurrence)
		case ErrorReadOnlyRootFilesystemFalse, ErrorReadOnlyRootFilesystemNil:
			resource = fixReadOnlyRootFilesystem(&result, resource, occurrence)
		case ErrorRunAsNonRootPSCTrueFalseCSCFalse, ErrorRunAsNonRootPSCNilCSCNil, ErrorRunAsNonRootPSCFalseCSCNil:
			resource = fixRunAsNonRoot(&result, resource, occurrence)
		case ErrorServiceAccountTokenDeprecated:
			resource = fixDeprecatedServiceAccount(resource)
		case ErrorAutomountServiceAccountTokenTrueAndNoName, ErrorAutomountServiceAccountTokenNilAndNoName:
			resource = fixServiceAccountToken(&result, resource)
		case ErrorAppArmorAnnotationMissing, ErrorAppArmorDisabled:
			resource = fixAppArmor(resource)
		case ErrorSeccompAnnotationMissing, ErrorSeccompDeprecated, ErrorSeccompDeprecatedPod, ErrorSeccompDisabled,
			ErrorSeccompDisabledPod:
			resource = fixSeccomp(resource)
		case ErrorMissingDefaultDenyIngressNetworkPolicy, ErrorMissingDefaultDenyEgressNetworkPolicy, ErrorMissingDefaultDenyIngressAndEgressNetworkPolicy:
			resource = fixNetworkPolicy(resource, occurrence)
		case ErrorNamespaceHostIPCTrue, ErrorNamespaceHostNetworkTrue, ErrorNamespaceHostPIDTrue:
			resource = fixNamespace(&result, resource)
		}
	}
	return resource
}

func prepareResourceForFix(resource Resource, result Result) Resource {
	needSecurityContextDefined := []int{ErrorAllowPrivilegeEscalationNil, ErrorAllowPrivilegeEscalationTrue,
		ErrorPrivilegedNil, ErrorPrivilegedTrue, ErrorReadOnlyRootFilesystemFalse, ErrorReadOnlyRootFilesystemNil,
		ErrorRunAsNonRootPSCTrueFalseCSCFalse, ErrorRunAsNonRootPSCNilCSCNil, ErrorRunAsNonRootPSCFalseCSCNil, ErrorServiceAccountTokenDeprecated,
		ErrorAutomountServiceAccountTokenTrueAndNoName, ErrorAutomountServiceAccountTokenNilAndNoName,
		ErrorCapabilityNotDropped, ErrorCapabilityAdded, ErrorMisconfiguredKubeauditAllow}
	needCapabilitiesDefined := []int{ErrorCapabilityNotDropped, ErrorCapabilityAdded, ErrorMisconfiguredKubeauditAllow}

	// Set of errors to fix
	errors := make(map[int]bool)
	for _, occurrence := range result.Occurrences {
		errors[occurrence.id] = true
	}

	for _, err := range needSecurityContextDefined {
		if _, ok := errors[err]; ok {
			resource = fixSecurityContextNil(resource)
			break
		}
	}

	for _, err := range needCapabilitiesDefined {
		if _, ok := errors[err]; ok {
			resource = fixCapabilitiesNil(resource)
			break
		}
	}

	return resource
}

func fix(resources []Resource) (fixedResources []Resource, extraResources []Resource) {
	for _, resource := range resources {
		if !IsSupportedResourceType(resource) {
			fixedResources = append(fixedResources, resource)
			continue
		}
		results := mergeAuditFunctions(getAuditFunctions())(resource)
		for _, result := range results {
			if IsNamespaceType(resource) {
				extraResource := fixPotentialSecurityIssue(resource, result)
				// If return resource from fixPotentialSecurityIssue is Namespace type then we don't have to add extra resources for it.
				if !IsNamespaceType(extraResource) {
					extraResources = append(extraResources, extraResource)
				}
			} else {
				resource = fixPotentialSecurityIssue(resource, result)
			}
		}
		fixedResources = append(fixedResources, resource)
	}
	return
}

// deepEqual recursively compares two values but ignores mapslice order and comments. For example the following values
// are considered to be equal:
//
//     []yaml.SequenceItem{{Value: yaml.MapSlice{
// 	       {Key: "k", Value: "v", Comment: "c"},
// 	       {Key: "k2", Value: "v2", Comment: "c2"},
//     }}}
//
//     []yaml.SequenceItem{{Value: yaml.MapSlice{
//          {Key: "k2", Value: "v2"},
//          {Key: "k", Value: "v"},
//      }}}

func deepEqual(val1, val2 interface{}) bool {
	// MapItem
	if mapItem1, ok := val1.(yaml.MapItem); ok {
		if mapItem2, ok := val2.(yaml.MapItem); ok {
			return mapItem1.Key == mapItem2.Key && deepEqual(mapItem1.Value, mapItem2.Value)
		}
		return false
	}

	// SequenceItem
	if seqItem1, ok := val1.(yaml.SequenceItem); ok {
		if seqItem2, ok := val2.(yaml.SequenceItem); ok {
			return deepEqual(seqItem1.Value, seqItem2.Value)
		}
		return false
	}

	// MapSlice
	if map1, ok := val1.(yaml.MapSlice); ok {
		if map2, ok := val2.(yaml.MapSlice); ok {
			numValues1, numValues2 := 0, 0
			for _, item1 := range map1 {
				if !isComment(item1) {
					numValues1++
				}
			}
			for _, item2 := range map2 {
				if !isComment(item2) {
					numValues2++
				}
			}
			if numValues1 != numValues2 {
				return false
			}
			for _, item1 := range map1 {
				if isComment(item1) {
					continue
				}
				item2, index2 := findItemInMapSlice(item1.Key, map2)
				if index2 == -1 || !deepEqual(item1.Value, item2.Value) {
					return false
				}
			}
			return true
		}
		return false
	}

	// []SequenceItem
	if seq1, ok := val1.([]yaml.SequenceItem); ok {
		if seq2, ok := val2.([]yaml.SequenceItem); ok {
			index1, index2 := 0, 0
			len1, len2 := len(seq1), len(seq2)
			for index1 < len1 || index2 < len2 {
				for index1 < len1 && isComment(seq1[index1]) {
					index1++
				}
				for index2 < len2 && isComment(seq1[index2]) {
					index2++
				}
				if (index1 == len1 && index2 < len2) || (index2 == len2 && index1 < len1) ||
					!deepEqual(seq1[index1].Value, seq2[index2].Value) {
					return false
				}
				index1++
				index2++
			}
			return true
		}
		return false
	}

	return val1 == val2
}

// isComment returns true if the value is a standalone comment (ie. not an end-of-line comment)
func isComment(val interface{}) bool {
	// MapItem
	if m, ok := val.(yaml.MapItem); ok {
		_, ok = m.Key.(yaml.PreDoc)
		return ok || (m.Key == nil && m.Value == nil && len(m.Comment) > 0)
	}

	// SequenceItem
	if s, ok := val.(yaml.SequenceItem); ok {
		return s.Value == nil && len(s.Comment) > 0
	}

	return false
}

// isFirstLineSeparator returns true if the first line in the manifest file is a yaml separator
func isFirstLineSeparatorOrComment(filename string) bool {
	file, err := os.Open(filename)
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lineStr := scanner.Text()
		if lineStr == "---" || lineStr[0] == '#' {
			return true
		}
		return false
	}
	return false
}

// isCommentSlice returns true if the byteslice contains only yaml comments
func isCommentSlice(b []byte) bool {
	lineSlice := bytes.Split(b, []byte("/n"))
	for _, line := range lineSlice {
		if len(line) > 0 && !strings.HasPrefix(string(line), "#") {
			return false
		}
	}
	return true
}

// equalValueForKey returns true if map1 and map2 have the same key-value pair for the given key
func equalValueForKey(key string, map1, map2 yaml.MapSlice) bool {
	if item1, index1 := findItemInMapSlice(key, map1); index1 != -1 {
		if item2, index2 := findItemInMapSlice(key, map2); index2 != -1 {
			return deepEqual(item1.Value, item2.Value)
		}
	}
	return false
}

// mergeYAML takes the file name of an autofixed YAML file (fixedFile) and the file name of the original YAML file
// (origFile) and merges fixedFile into origFile such that the resulting byte array is autofixed YAML but with the
// same order and comments as the original.
func mergeYAML(origFile, fixedFile string) ([]byte, error) {
	origData, err := ioutil.ReadFile(origFile)
	if err != nil {
		return nil, err
	}
	origYaml, err := yaml.CommentUnmarshal(origData)
	if err != nil {
		return nil, err
	}
	fixedData, err := ioutil.ReadFile(fixedFile)
	if err != nil {
		return nil, err
	}
	fixedYaml, err := yaml.CommentUnmarshal(fixedData)
	if err != nil {
		return nil, err
	}

	// Take out post-doc comments
	commentStart := len(origYaml)
	for origYaml[commentStart-1].Key == nil && len(origYaml[commentStart-1].Comment) > 0 {
		commentStart--
	}
	comments := make(yaml.MapSlice, 0, len(origYaml)-commentStart)
	comments = append(comments, origYaml[commentStart:]...)
	origYaml = origYaml[:commentStart]

	// Merge fixed YAML into original YAML
	mergedYaml := mergeMapSlices(origYaml, fixedYaml)

	// Put back post-doc comments
	mergedYaml = append(mergedYaml, comments...)

	// Convert YAML to byte array
	data, err := yaml.Marshal(&mergedYaml)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// mergeMapSlices recursively merges fixedSlice and origSlice.
// Keys which exist in origSlice but not fixedSlice are excluded.
// Keys which exist in fixedSlice but not origSlice are included.
// If keys exist in both fixedSlice and origSlice then the value from fixedSlice is used unless both values are complex
// (MapSlices or SequenceItem arrays), in which case they are merged recursively.
func mergeMapSlices(origSlice, fixedSlice yaml.MapSlice) yaml.MapSlice {
	var mergedSlice yaml.MapSlice

	// Keep comments, and items which are present in both the original and fixed yaml
	for _, item := range origSlice {
		if _, index := findItemInMapSlice(item.Key, fixedSlice); index != -1 || isComment(item) {
			mergedSlice = append(mergedSlice, item)
			continue
		}
	}

	// Update or add items from the fixed yaml which are not in the original
	for _, fixedItem := range fixedSlice {
		_, mergedItemIndex := findItemInMapSlice(fixedItem.Key, mergedSlice)
		if mergedItemIndex == -1 {
			mergedSlice = append(mergedSlice, fixedItem)
			continue
		}

		mergedItem := &mergedSlice[mergedItemIndex]
		if fixedMap, ok := fixedItem.Value.(yaml.MapSlice); ok {
			if origMap, ok := mergedItem.Value.(yaml.MapSlice); ok {
				mergedItem.Value = mergeMapSlices(origMap, fixedMap)
				continue
			}
		}
		if fixedSeq, ok := fixedItem.Value.([]yaml.SequenceItem); ok {
			if origSeq, ok := mergedItem.Value.([]yaml.SequenceItem); ok {
				mergedItem.Value = mergeSequences(mergedItem.Key.(string), origSeq, fixedSeq)
				continue
			}
		}
		mergedItem.Value = fixedItem.Value
	}

	return mergedSlice
}

// mergeSequences recursively merges fixedSlice and origSlice.
// Values which exist in origSlice but not fixedSlice are excluded.
// Values which exist in fixedSlice but not origSlice are included.
// If values exist in both fixedSlice and origSlice then the value from fixedSlice is used unless both values are
// complex (MapSlices or SequenceItem arrays), in which case they are merged recursively.
func mergeSequences(sequenceKey string, origSlice, fixedSlice []yaml.SequenceItem) []yaml.SequenceItem {
	var mergedSlice []yaml.SequenceItem

	// Keep comments, and items which are present in both the original and fixed yaml
	for _, item := range origSlice {
		if _, index := findItemInSequence(sequenceKey, item, fixedSlice); index != -1 || isComment(item) {
			mergedSlice = append(mergedSlice, item)
		}
	}

	// Update or add items from the fixed yaml which are not in the original
	for _, fixedItem := range fixedSlice {
		_, mergedItemIndex := findItemInSequence(sequenceKey, fixedItem, mergedSlice)
		if mergedItemIndex == -1 {
			mergedSlice = append(mergedSlice, fixedItem)
			continue
		}

		mergedItem := &mergedSlice[mergedItemIndex]
		if _, ok := fixedItem.Value.(yaml.MapSlice); ok {
			if _, ok = mergedItem.Value.(yaml.MapSlice); ok {
				mergedItem.Value = mergeMapSlices(mergedItem.Value.(yaml.MapSlice), fixedItem.Value.(yaml.MapSlice))
				continue
			}
		}
		mergedItem.Value = fixedItem.Value
	}

	return mergedSlice
}

// findItemInMapSlice returns the item in the MapSlice with the given key, and its index
func findItemInMapSlice(key interface{}, slice yaml.MapSlice) (yaml.MapItem, int) {
	for i, item := range slice {
		if item.Key != nil && deepEqual(item.Key, key) {
			return item, i
		}
	}
	return yaml.MapItem{}, -1
}

// findItemInSequence returns the item in slice which "matches" val and its index. See sequenceItemMatch for what
// is considered a "match".
func findItemInSequence(sequenceKey string, val yaml.SequenceItem, slice []yaml.SequenceItem) (yaml.SequenceItem, int) {
	for i, item := range slice {
		if item.Value != nil && sequenceItemMatch(sequenceKey, val, item) {
			return item, i
		}
	}
	return yaml.SequenceItem{}, -1
}

var identifyingKey = map[string]string{
	"allowedFlexVolumes": "driver",     // PodSecurityPolicySpec.allowedFlexVolumes : AllowedFlexVolume.driver
	"allowedHostPaths":   "pathPrefix", // PodSecurityPolicySpec.allowedHostPaths : AllowedHostPath.pathPrefix
	// StorageClass.allowedTopologies : TopologySelectorTerm.matchLabelExpressions
	"allowedTopologies":    "matchLabelExpressions",
	"clusterRoleSelectors": "matchExpressions", // AggregationRule.clusterRoleSelectors : LabelSelector.matchExpressions
	"containers":           "name",             // PodSpec.contaienrs : Container.name
	"egress":               "ports",            // NetworkPolicySpec.egress : NetworkPolicyEgressRule.ports
	"env":                  "name",             // Container.env : EnvVar.name
	"hostAliases":          "ip",               // PodSpec.hostAliases : HostAlias.ip
	// Assumes it is not possible to add multiple values for the same header, ie.
	//     httpHeaders:
	//         - name: header1
	//           value: value1
	//         - name: header1
	//           value: value2
	// This restriction is not documented so the assumption may be incorrect
	// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.13/#httpheader-v1-core
	"httpHeaders": "name", // HTTPGetAction.httpHeaders : HTTPHeader.name
	// PodSpec.imagePullSecrets : LocalObjectReference.name
	// ServiceAccount.imagePullSecrets : LocalObjectReference.name
	"imagePullSecrets": "name",
	"initContainers":   "name", // PodSpec.initContainers : Container.name
	// LabelSelector.matchExpressions : LabelSelectorRequirement.key
	// NodeSelectorTerm.matchExpressions : NodeSelectorRequirement.key
	"matchExpressions": "key",
	"matchFields":      "key",  // NodeSelectorTerm.matchFields : NodeSelectorRequirement.key
	"options":          "name", // PodDNSConfig.options : PodDNSConfigOption.name
	// TopologySelectorTerm.matchLabelExpressions : TopologySelectorLabelRequirement.key
	"matchLabelExpressions": "key",
	"pending":               "name",          // Initializers.pending : Initializer.name
	"readinessGates":        "conditionType", // PodSpec.readinessGates : PodReadinessGate.conditionType
	// PodAffinity.requiredDuringSchedulingIgnoredDuringExecution : PodAffinityTerm.labelSelector
	// PodAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution : PodAffinityTerm.labelSelector
	"requiredDuringSchedulingIgnoredDuringExecution": "labelSelector",
	"secrets": "name", // ServiceAccount.secrets : ObjectReference.name
	// ClusterRoleBinding.subjects : Subject.name
	// RoleBinding.subjects : Subject.name
	"subjects":      "name",
	"subsets":       "addresses",  // Endpoints.subsets : EndpointSubset.addresses
	"sysctls":       "name",       // PodSecurityContext.sysctls : Sysctl.name
	"taints":        "key",        // NodeSpec.taints : Taint.key
	"volumeDevices": "devicePath", // Container.volumeDevices : VolumeDevice.devicePath
	"volumeMounts":  "mountPath",  // Container.volumeMounts : VolumeMount.mountPath
	"volumes":       "name",       // PodSpec.volumes : Volume.name
}

// sequenceItemMatch returns true if item1 and item2 are a match, false otherwise. In order to determine whether
// sequence items match (and should be merged) we determine the "identifying key" for the sequence item, and if both
// sequence items have the same key-value pair for the "identifying key" then they are a match. The sequenceKey
// is the key for which the array items are the value. ie:
//     sequenceKey:
//     - item1
//     - item2
func sequenceItemMatch(sequenceKey string, item1, item2 yaml.SequenceItem) bool {
	switch item1.Value.(type) {
	case string, int, bool:
		return item1.Value == item2.Value
	}
	map1, ok1 := item1.Value.(yaml.MapSlice)
	map2, ok2 := item2.Value.(yaml.MapSlice)
	if !ok1 || !ok2 {
		return false
	}

	switch sequenceKey {
	// EndpointSubset.addresses : EndpointAddress.[hostname OR ip]
	// EndpointSubset.notReadyAddresses : EndpointAddress.[hostname OR ip]
	case "addresses", "notReadyAddresses":
		if equalValueForKey("hostname", map1, map2) {
			return true
		}
		return equalValueForKey("ip", map1, map2)

	// Container.envFrom : EnvFromSource.[configMapRef OR secretRef].name
	case "envFrom":
		if val1, index1 := findItemInMapSlice("configMapRef", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("configMapRef", map2); index2 != -1 {
				return equalValueForKey("name", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
		}
		if val1, index1 := findItemInMapSlice("secretRef", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("secretRef", map2); index2 != -1 {
				return equalValueForKey("name", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
		}
		return false

	// NetworkPolicySpec.ingress : NetworkPolicyIngressRule.[ports OR from]
	case "ingress":
		if equalValueForKey("ports", map1, map2) {
			return true
		}
		return equalValueForKey("from", map1, map2)

	// ConfigMapProjection.items : KeyToPath.key
	// ConfigMapVolumeSource.items : KeyToPath.key
	// DownwardAPIVolumeSource.items : DownwardAPIVolumeFile.path
	// SecretSecretProjection.items : KeyToPath.key
	// SecretVolumeSource.items : KeyToPath.key
	case "items":
		// ConfigMapVolumeSource.items : KeyToPath.key
		// SecretVolumeSource.items : KeyToPath.key
		if equalValueForKey("key", map1, map2) {
			return true
		}
		// DownwardAPIVolumeSource.items : DownwardAPIVolumeFile.path
		return equalValueForKey("path", map1, map2)

	// NodeSelector.nodeSelectorTerms : NodeSelectorTerm.[matchExpressions OR matchFields]
	case "nodeSelectorTerms":
		// This is a bit of a complicated case.
		// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.13/#nodeselector-v1-core
		// For now, only match if there is an exact match for the complex value of either the "matchExpressions" or
		// "matchFields" fields.
		if equalValueForKey("matchExpressions", map1, map2) {
			return true
		}
		return equalValueForKey("matchFields", map1, map2)

	// ObjectMeta.ownerReferences : OwnerReference.[uid OR name]
	case "ownerReferences":
		if equalValueForKey("uid", map1, map2) {
			return true
		}
		return equalValueForKey("name", map1, map2)

	// NodeAffinity.preferredDuringSchedulingIgnoredDuringExecution : PreferredSchedulingTerm.preference
	// PodAffinity.preferredDuringSchedulingIgnoredDuringExecution : WeightedPodAffinityTerm.podAffinityTerm
	// PodAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution : WeightedPodAffinityTerm.podAffinityTerm
	case "preferredDuringSchedulingIgnoredDuringExecution":
		// This is a bit of a complicated case as the values are very nested and because the same identifying key is
		// used for two different array types (PreferredSchedulingTerm and WeightedPodAffinityTerm).
		// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.13/#nodeaffinity-v1-core
		// and https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.13/#podaffinity-v1-core
		// For now, only match if there is an exact match for the complex value of the "preference" or
		// "podAffinityTerm" field.
		// The value for the "weight" field can be updated.

		// NodeAffinity.preferredDuringSchedulingIgnoredDuringExecution : PreferredSchedulingTerm.preference
		if equalValueForKey("preference", map1, map2) {
			return true
		}
		// PodAffinity.preferredDuringSchedulingIgnoredDuringExecution : WeightedPodAffinityTerm.podAffinityTerm
		// PodAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution : WeightedPodAffinityTerm.podAffinityTerm
		return equalValueForKey("podAffinityTerm", map1, map2)

	// Container.ports : ContainerPort.containerPort
	// EndpointSubset.ports : EndpointPort.port
	// ServiceSpec.ports : ServicePort.port
	case "ports":
		// Container.ports : ContainerPort.containerPort
		if equalValueForKey("containerPort", map1, map2) {
			return true
		}
		// EndpointSubset.ports : EndpointPort.port
		// ServiceSpec.ports : ServicePort.port
		return equalValueForKey("port", map1, map2)

	// ClusterRole.rules : PolicyRule.resources
	// IngressSpec.rules : IngressRule.host
	// Role.rules : PolicyRule.resources
	case "rules":
		// ClusterRole.rules : PolicyRule.resources
		// Role.rules : PolicyRule.resources
		if equalValueForKey("resources", map1, map2) {
			return true
		}

		// IngressSpec.rules : IngressRule.host
		if equalValueForKey("host", map1, map2) {
			return true
		}

	// ProjectedVolumeSource.sources
	case "sources":
		// ProjectedVolumeSource.sources : VolumeProjection.configMap.name
		if val1, index1 := findItemInMapSlice("configMap", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("configMap", map2); index2 != -1 {
				return equalValueForKey("name", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
			return false
		}
		// ProjectedVolumeSource.sources : VolumeProjection.downwardAPI.items
		if val1, index1 := findItemInMapSlice("downwardAPI", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("downwardAPI", map2); index2 != -1 {
				return equalValueForKey("items", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
			return false
		}
		// ProjectedVolumeSource.sources : VolumeProjection.secret.name
		if val1, index1 := findItemInMapSlice("secret", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("secret", map2); index2 != -1 {
				return equalValueForKey("name", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
			return false
		}
		// ProjectedVolumeSource.sources : VolumeProjection.serviceAccountToken.name
		if val1, index1 := findItemInMapSlice("serviceAccountToken", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("serviceAccountToken", map2); index2 != -1 {
				return equalValueForKey("path", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
		}
		return false

	// IngressSpec.tls : IngressTLS.[secretName OR hosts]
	case "tls":
		if equalValueForKey("secretName", map1, map2) {
			return true
		}
		return equalValueForKey("hosts", map1, map2)

	// StatefulSetSpec.volumeClaimTemplates : PersistentVolumeClaim.metadata.name
	case "volumeClaimTemplates":
		if val1, index1 := findItemInMapSlice("metadata", map1); index1 != -1 {
			if val2, index2 := findItemInMapSlice("metadata", map2); index2 != -1 {
				return equalValueForKey("name", val1.Value.(yaml.MapSlice), val2.Value.(yaml.MapSlice))
			}
		}
		return false
	}

	if idKey, ok := identifyingKey[sequenceKey]; ok {
		return equalValueForKey(idKey, map1, map2)
	}

	// FSGroupStrategyOptions.ranges : IDRange
	// RunAsGroupStrategyOptions.ranges : IDRange
	// RunAsUserStrategyOptions.ranges : IDRange
	// SupplementalGroupsStrategyOptions.ranges : IDRange
	// PodSecurityPolicySpec.hostPorts : HostPortRange
	// PodSpec.tolerations : Toleration
	return deepEqual(map1, map2)
}

// SplitYamlResource splits the yaml file into byte slices for each resource in the yaml file and checks if the first resource
// is only comments, in which case it deletes the first resource in the slice and add's the comment to the final file and updates toAppend flag

func splitYamlResources(filename string, toWriteFile string) (splitDecoded [][]byte, toAppend bool, err error) {
	buf, err := ioutil.ReadFile(rootConfig.manifest)

	if err != nil {
		log.Error("File not found")
		return
	}
	splitDecoded = bytes.Split(buf, []byte("---"))
	if err != nil {
		log.Error(err)
		return nil, false, err
	}
	if len(splitDecoded) != 0 {
		if len(splitDecoded[0]) == 0 {
			splitDecoded = splitDecoded[1:]
		}
		decoder := scheme.Codecs.UniversalDeserializer()
		_, _, err := decoder.Decode(splitDecoded[0], nil, nil)
		// if Decode returns err, then it means that splitDecoded[0] is only comments(pre doc) in this case remove this resource from slice and write to file
		if err != nil {
			err = writeManifestFile(splitDecoded[0], toWriteFile, false)
			if err != nil {
				return nil, false, err
			}
			splitDecoded = splitDecoded[1:]
			return splitDecoded, true, nil
		}
	}

	return splitDecoded, false, nil
}

func cleanupManifest(origFile string, finalData []byte) ([]byte, error) {
	objectMetacreationTs := []byte("\n  creationTimestamp: null\n")
	specTemplatecreationTs := []byte("\n      creationTimestamp: null\n")
	jobSpecTemplatecreationTs := []byte("\n          creationTimestamp: null\n")
	nullStatus := []byte("\nstatus: {}\n")
	nullReplicaStatus := []byte("status:\n  replicas: 0\n")
	nullLBStatus := []byte("status:\n  loadBalancer: {}\n")
	nullMetaStatus := []byte("\n    status: {}\n")

	var hasObjectMetacreationTs, hasSpecTemplatecreationTs, hasJobSpecTemplatecreationTs, hasNullStatus,
		hasNullReplicaStatus, hasNullLBStatus, hasNullMetaStatus bool

	if origFile != "" {
		origData, err := ioutil.ReadFile(origFile)
		if err != nil {
			return nil, err
		}
		hasObjectMetacreationTs = bytes.Contains(origData, objectMetacreationTs)
		hasSpecTemplatecreationTs = bytes.Contains(origData, specTemplatecreationTs)
		hasJobSpecTemplatecreationTs = bytes.Contains(origData, jobSpecTemplatecreationTs)

		hasNullStatus = bytes.Contains(origData, nullStatus)
		hasNullReplicaStatus = bytes.Contains(origData, nullReplicaStatus)
		hasNullLBStatus = bytes.Contains(origData, nullLBStatus)
		hasNullMetaStatus = bytes.Contains(origData, nullMetaStatus)

	} // null value is false in case of origFile

	if !hasObjectMetacreationTs {
		finalData = bytes.Replace(finalData, objectMetacreationTs, []byte("\n"), -1)
	}
	if !hasSpecTemplatecreationTs {
		finalData = bytes.Replace(finalData, specTemplatecreationTs, []byte("\n"), -1)
	}
	if !hasJobSpecTemplatecreationTs {
		finalData = bytes.Replace(finalData, jobSpecTemplatecreationTs, []byte("\n"), -1)
	}
	if !hasNullStatus {
		finalData = bytes.Replace(finalData, nullStatus, []byte("\n"), -1)
	}
	if !hasNullReplicaStatus {
		finalData = bytes.Replace(finalData, nullReplicaStatus, []byte("\n"), -1)
	}
	if !hasNullLBStatus {
		finalData = bytes.Replace(finalData, nullLBStatus, []byte("\n"), -1)
	}
	if !hasNullMetaStatus {
		finalData = bytes.Replace(finalData, nullMetaStatus, []byte("\n"), -1)
	}

	return finalData, nil
}
