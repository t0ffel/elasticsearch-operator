package k8shandler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	v1alpha1 "github.com/ViaQ/elasticsearch-operator/pkg/apis/elasticsearch/v1alpha1"
	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	elasticsearchCertsPath    = "/etc/elasticsearch/secret"
	elasticsearchConfigPath   = "/usr/share/java/elasticsearch/config"
	elasticsearchDefaultImage = "docker.io/t0ffel/elasticsearch5"
	heapDumpLocation          = "/elasticsearch/persistent/heapdump.hprof"
	promUser                  = "prometheus"
)

type nodeState struct {
	Desired desiredNodeState
	Actual  actualNodeState
}

type desiredNodeState struct {
	ClusterName         string
	Namespace           string
	DeployName          string
	Roles               []v1alpha1.ElasticsearchNodeRole
	ESNodeSpec          v1alpha1.ElasticsearchNode
	ElasticsearchSecure v1alpha1.ElasticsearchSecure
	NodeNum             int32
	ReplicaNum          int32
	ServiceAccountName  string
	ConfigMapName       string
}

type actualNodeState struct {
	StatefulSet *apps.StatefulSet
	Deployment  *apps.Deployment
	ReplicaSet  *apps.ReplicaSet
	Pod         *v1.Pod
}

func constructNodeSpec(dpl *v1alpha1.Elasticsearch, esNode v1alpha1.ElasticsearchNode, configMapName, serviceAccountName string, nodeNum int32, replicaNum int32) (desiredNodeState, error) {
	nodeCfg := desiredNodeState{
		ClusterName:         dpl.Name,
		Namespace:           dpl.Namespace,
		Roles:               esNode.Roles,
		ESNodeSpec:          esNode,
		ElasticsearchSecure: dpl.Spec.Secure,
		NodeNum:             nodeNum,
		ReplicaNum:          replicaNum,
		ServiceAccountName:  serviceAccountName,
		ConfigMapName:       configMapName,
	}
	deployName, err := constructDeployName(dpl.Name, esNode.Roles, nodeNum, replicaNum)
	if err != nil {
		return nodeCfg, err
	}
	nodeCfg.DeployName = deployName

	nodeCfg.ESNodeSpec.Spec = reconcileNodeSpec(dpl.Spec.Spec, esNode.Spec)
	return nodeCfg, nil
}

func constructDeployName(name string, roles []v1alpha1.ElasticsearchNodeRole, nodeNum int32, replicaNum int32) (string, error) {
	if len(roles) == 0 {
		return "", fmt.Errorf("No node roles specified for a node in cluster %s", name)
	}
	var nodeType []string
	for _, role := range roles {
		if role != "client" && role != "data" && role != "master" {
			return "", fmt.Errorf("Unknown node's role: %s", role)
		}
		nodeType = append(nodeType, string(role))
	}

	sort.Strings(nodeType)

	return fmt.Sprintf("%s-%s-%d-%d", name, strings.Join(nodeType, ""), nodeNum, replicaNum), nil
}

func reconcileNodeSpec(commonSpec, nodeSpec v1alpha1.ElasticsearchNodeSpec) v1alpha1.ElasticsearchNodeSpec {
	var image string
	if nodeSpec.Image == "" {
		image = commonSpec.Image
	} else {
		image = nodeSpec.Image
	}
	nodeSpec = v1alpha1.ElasticsearchNodeSpec{
		Image:     image,
		Resources: getResourceRequirements(commonSpec.Resources, nodeSpec.Resources),
	}
	return nodeSpec
}

// getReplicas returns the desired number of replicas in the deployment/statefulset
// if this is a data deployment, we always want to create separate deployment per replica
// so we'll return 1. if this is not a data node, we can simply scale existing replica.
func (cfg *desiredNodeState) getReplicas() int32 {
	if cfg.isNodeData() {
		return 1
	}
	return cfg.ESNodeSpec.Replicas
}

func (cfg *desiredNodeState) isNodeMaster() bool {
	for _, role := range cfg.Roles {
		if role == "master" {
			return true
		}
	}
	return false
}

func (cfg *desiredNodeState) isNodeData() bool {
	for _, role := range cfg.Roles {
		if role == "data" {
			return true
		}
	}
	return false
}

func (cfg *desiredNodeState) isNodeClient() bool {
	for _, role := range cfg.Roles {
		if role == "client" {
			return true
		}
	}
	return false
}

func (cfg *desiredNodeState) getLabels() map[string]string {
	return map[string]string{
		"component": fmt.Sprintf("elasticsearch-%s", cfg.ClusterName),
		//"es-node-role":   cfg.NodeType,
		"es-node-client": strconv.FormatBool(cfg.isNodeClient()),
		"es-node-data":   strconv.FormatBool(cfg.isNodeData()),
		"es-node-master": strconv.FormatBool(cfg.isNodeMaster()),
		"cluster":        cfg.ClusterName,
	}
}

func (cfg *desiredNodeState) getNode() NodeTypeInterface {
	if cfg.isNodeData() {
		return NewDeploymentNode(cfg.DeployName, cfg.Namespace)
	}
	return NewStatefulSetNode(cfg.DeployName, cfg.Namespace)
}

func (cfg *desiredNodeState) CreateOrUpdateNode(owner metav1.OwnerReference) error {
	node := cfg.getNode()
	err := node.query()
	if err != nil {
		// Node's resource doesn't exist, we can construct one
		logrus.Infof("Constructing new resource %v", cfg.DeployName)
		dep, err := node.constructNodeResource(cfg, owner)
		if err != nil {
			return fmt.Errorf("Could not construct node resource: %v", err)
		}
		err = sdk.Create(dep)
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("Could not create node resource: %v", err)
		}
		return nil
	}

	// TODO: what is allowed to be changed in the StatefulSet ?
	// Validate Elasticsearch cluster parameters
	diff, err := node.isDifferent(cfg)
	if err != nil {
		return fmt.Errorf("Failed to see if the node resource is different from what's needed: %v", err)
	}

	if diff {
		dep, err := node.constructNodeResource(cfg, metav1.OwnerReference{})
		if err != nil {
			return fmt.Errorf("Could not construct node resource for update: %v", err)
		}
		logrus.Infof("Updating node resource %v", cfg.DeployName)
		err = sdk.Update(dep)
		if err != nil {
			return fmt.Errorf("Failed to update node resource: %v", err)
		}
	}
	return nil
}

func (cfg *desiredNodeState) IsUpdateNeeded() bool {
	// FIXME: to be refactored. query() must not exist here, since
	// we already have information in clusterState
	node := cfg.getNode()
	err := node.query()
	if err != nil {
		// resource doesn't exist, so the update is needed
		return true
	}

	diff, err := node.isDifferent(cfg)
	if err != nil {
		logrus.Errorf("Failed to obtain if there is a significant difference in resources: %v", err)
		return false
	}

	if diff {
		return true
	}
	return false
}

func (node *nodeState) setDeployment(deployment apps.Deployment) {
	node.Actual.Deployment = &deployment
}

func (node *nodeState) setReplicaSet(replicaSet apps.ReplicaSet) {
	node.Actual.ReplicaSet = &replicaSet
}

func (node *nodeState) setPod(pod v1.Pod) {
	node.Actual.Pod = &pod
}

func (cfg *desiredNodeState) getAffinity() v1.Affinity {
	labelSelectorReqs := []metav1.LabelSelectorRequirement{}
	if cfg.isNodeClient() {
		labelSelectorReqs = append(labelSelectorReqs, metav1.LabelSelectorRequirement{
			Key:      "es-node-client",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"true"},
		})
	}
	if cfg.isNodeData() {
		labelSelectorReqs = append(labelSelectorReqs, metav1.LabelSelectorRequirement{
			Key:      "es-node-data",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"true"},
		})
	}
	if cfg.isNodeMaster() {
		labelSelectorReqs = append(labelSelectorReqs, metav1.LabelSelectorRequirement{
			Key:      "es-node-master",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"true"},
		})
	}

	return v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: labelSelectorReqs,
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

func (cfg *desiredNodeState) getEnvVars() []v1.EnvVar {
	return []v1.EnvVar{
		v1.EnvVar{
			Name:  "DC_NAME",
			Value: cfg.DeployName,
		},
		v1.EnvVar{
			Name: "NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		v1.EnvVar{
			Name:  "KUBERNETES_TRUST_CERT",
			Value: "true",
		},
		v1.EnvVar{
			Name:  "SERVICE_DNS",
			Value: fmt.Sprintf("%s-cluster", cfg.ClusterName),
		},
		v1.EnvVar{
			Name:  "CLUSTER_NAME",
			Value: cfg.ClusterName,
		},
		v1.EnvVar{
			Name:  "INSTANCE_RAM",
			Value: cfg.getInstanceRAM(),
		},
		v1.EnvVar{
			Name:  "HEAP_DUMP_LOCATION",
			Value: heapDumpLocation,
		},
		v1.EnvVar{
			Name:  "NODE_QUORUM",
			Value: "1",
		},
		v1.EnvVar{
			Name:  "RECOVER_EXPECTED_NODES",
			Value: "1",
		},
		v1.EnvVar{
			Name:  "RECOVER_AFTER_TIME",
			Value: "5m",
		},
		v1.EnvVar{
			Name:  "READINESS_PROBE_TIMEOUT",
			Value: "30",
		},
		v1.EnvVar{
			Name:  "POD_LABEL",
			Value: fmt.Sprintf("cluster=%s", cfg.ClusterName),
		},
		v1.EnvVar{
			Name:  "IS_MASTER",
			Value: strconv.FormatBool(cfg.isNodeMaster()),
		},
		v1.EnvVar{
			Name:  "HAS_DATA",
			Value: strconv.FormatBool(cfg.isNodeData()),
		},
		v1.EnvVar{
			Name:  "PROMETHEUS_USER",
			Value: promUser,
		},
		v1.EnvVar{
			Name:  "PRIMARY_SHARDS",
			Value: "1",
		},
		v1.EnvVar{
			Name:  "REPLICA_SHARDS",
			Value: "0",
		},
	}
}

func (cfg *desiredNodeState) getInstanceRAM() string {
	memory := cfg.ESNodeSpec.Spec.Resources.Limits.Memory()
	if !memory.IsZero() {
		return memory.String()
	}
	return defaultMemoryLimit
}

func (cfg *desiredNodeState) getESContainer() v1.Container {
	var image string
	if cfg.ESNodeSpec.Spec.Image == "" {
		image = elasticsearchDefaultImage
	} else {
		image = cfg.ESNodeSpec.Spec.Image
	}
	probe := getReadinessProbe()
	return v1.Container{
		Name:            "elasticsearch",
		Image:           image,
		ImagePullPolicy: "IfNotPresent",
		Env:             cfg.getEnvVars(),
		Ports: []v1.ContainerPort{
			v1.ContainerPort{
				Name:          "cluster",
				ContainerPort: 9300,
				Protocol:      v1.ProtocolTCP,
			},
			v1.ContainerPort{
				Name:          "restapi",
				ContainerPort: 9200,
				Protocol:      v1.ProtocolTCP,
			},
		},
		ReadinessProbe: &probe,
		LivenessProbe:  &probe,
		VolumeMounts:   cfg.getVolumeMounts(),
		Resources:      cfg.ESNodeSpec.Spec.Resources,
	}
}

func (cfg *desiredNodeState) getVolumeMounts() []v1.VolumeMount {
	mounts := []v1.VolumeMount{
		v1.VolumeMount{
			Name:      "elasticsearch-storage",
			MountPath: "/elasticsearch/persistent",
		},
		v1.VolumeMount{
			Name:      "elasticsearch-config",
			MountPath: elasticsearchConfigPath,
		},
	}
	if !cfg.ElasticsearchSecure.Disabled {
		mounts = append(mounts, v1.VolumeMount{
			Name:      "certificates",
			MountPath: elasticsearchCertsPath,
		})
	}
	return mounts
}

func (cfg *desiredNodeState) generatePersistentStorage() v1.VolumeSource {
	volSource := v1.VolumeSource{}
	specVol := cfg.ESNodeSpec.Storage
	switch {
	case specVol.HostPath != nil:
		volSource.HostPath = specVol.HostPath
	case specVol.EmptyDir != nil || specVol == v1alpha1.ElasticsearchNodeStorageSource{}:
		volSource.EmptyDir = specVol.EmptyDir
	case specVol.VolumeClaimTemplate != nil:
		claimName := fmt.Sprintf("%s-%s", specVol.VolumeClaimTemplate.Name, cfg.DeployName)
		volClaim := v1.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName,
		}
		volSource.PersistentVolumeClaim = &volClaim
		err := createOrUpdatePersistentVolumeClaim(specVol.VolumeClaimTemplate.Spec, claimName, cfg.Namespace)
		if err != nil {
			logrus.Errorf("Unable to create PersistentVolumeClaim: %v", err)
		}
	case specVol.PersistentVolumeClaim != nil:
		volSource.PersistentVolumeClaim = specVol.PersistentVolumeClaim
	default:
		// TODO: assume EmptyDir/update to emptyDir?
		logrus.Infof("Unknown volume source: %s", specVol)
	}
	return volSource
}

func (cfg *desiredNodeState) getVolumes() []v1.Volume {
	vols := []v1.Volume{
		v1.Volume{
			Name:         "elasticsearch-storage",
			VolumeSource: cfg.generatePersistentStorage(),
		},
		v1.Volume{
			Name: "elasticsearch-config",
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: cfg.ConfigMapName,
					},
				},
			},
		},
	}
	if !cfg.ElasticsearchSecure.Disabled {
		var secretName string
		if cfg.ElasticsearchSecure.CertificatesSecret == "" {
			secretName = cfg.ClusterName
		} else {
			secretName = cfg.ElasticsearchSecure.CertificatesSecret
		}

		vols = append(vols, v1.Volume{
			Name: "certificates",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: secretName,
				},
			},
		})
	}
	return vols

}

func (cfg *desiredNodeState) getSelector() (map[string]string, bool) {
	if len(cfg.ESNodeSpec.NodeSelector) == 0 {
		return nil, false
	}
	return cfg.ESNodeSpec.NodeSelector, true
}

func (actualState *actualNodeState) isStatusUpdateNeeded(nodesInStatus v1alpha1.ElasticsearchStatus) bool {
	if actualState.Deployment == nil {
		return false
	}
	for _, node := range nodesInStatus.Nodes {
		if actualState.Deployment.Name == node.DeploymentName {
			if actualState.ReplicaSet == nil {
				return false
			}
			// This is the proper item in the array of node statuses
			if actualState.ReplicaSet.Name != node.ReplicaSetName {
				return true
			}

			if actualState.Pod == nil {
				return false
			}

			if actualState.Pod.Name != node.PodName || string(actualState.Pod.Status.Phase) != node.Status {
				return true
			}
			return false

		}
	}

	// no corresponding nodes in status
	return true
}
