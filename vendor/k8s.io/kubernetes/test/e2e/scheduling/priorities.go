/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduling

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/onsi/ginkgo"

	// ensure libs have a chance to initialize
	_ "github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2erc "k8s.io/kubernetes/test/e2e/framework/rc"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

// Resource is a collection of compute resource.
type Resource struct {
	MilliCPU int64
	Memory   int64
}

var balancePodLabel = map[string]string{"podname": "priority-balanced-memory"}

// track min memory limit based on crio minimum. pods cannot set a limit lower than this
// see: https://github.com/cri-o/cri-o/blob/29805b13e9a43d9d22628553db337ce1c1bec0a8/internal/config/cgmgr/cgmgr.go#L23
// see: https://bugzilla.redhat.com/show_bug.cgi?id=1595256
var crioMinMemLimit = 12 * 1024 * 1024

var podRequestedResource = &v1.ResourceRequirements{
	Limits: v1.ResourceList{
		v1.ResourceMemory: resource.MustParse("100Mi"),
		v1.ResourceCPU:    resource.MustParse("100m"),
	},
	Requests: v1.ResourceList{
		v1.ResourceMemory: resource.MustParse("100Mi"),
		v1.ResourceCPU:    resource.MustParse("100m"),
	},
}

// addOrUpdateAvoidPodOnNode adds avoidPods annotations to node, will override if it exists
func addOrUpdateAvoidPodOnNode(c clientset.Interface, nodeName string, avoidPods v1.AvoidPods) {
	err := wait.PollImmediate(framework.Poll, framework.SingleCallTimeout, func() (bool, error) {
		node, err := c.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		taintsData, err := json.Marshal(avoidPods)
		framework.ExpectNoError(err)

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[v1.PreferAvoidPodsAnnotationKey] = string(taintsData)
		_, err = c.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		if err != nil {
			if !apierrors.IsConflict(err) {
				framework.ExpectNoError(err)
			} else {
				framework.Logf("Conflict when trying to add/update avoidPods %v to %v with error %v", avoidPods, nodeName, err)
				return false, nil
			}
		}
		return true, nil
	})
	framework.ExpectNoError(err)
}

// removeAvoidPodsOffNode removes AvoidPods annotations from the node. It does not fail if no such annotation exists.
func removeAvoidPodsOffNode(c clientset.Interface, nodeName string) {
	err := wait.PollImmediate(framework.Poll, framework.SingleCallTimeout, func() (bool, error) {
		node, err := c.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if node.Annotations == nil {
			return true, nil
		}
		delete(node.Annotations, v1.PreferAvoidPodsAnnotationKey)
		_, err = c.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		if err != nil {
			if !apierrors.IsConflict(err) {
				framework.ExpectNoError(err)
			} else {
				framework.Logf("Conflict when trying to remove avoidPods to %v", nodeName)
				return false, nil
			}
		}
		return true, nil
	})
	framework.ExpectNoError(err)
}

// nodesAreTooUtilized ensures that each node can support 2*crioMinMemLimit
// We check for double because it needs to support at least the cri-o minimum
// plus whatever delta between node usages (which could be up to or at least crioMinMemLimit)
func nodesAreTooUtilized(cs clientset.Interface, nodeList *v1.NodeList) bool {
	for _, node := range nodeList.Items {
		_, memFraction, _, memAllocatable := computeCPUMemFraction(cs, node, podRequestedResource)
		if float64(memAllocatable)-(memFraction*float64(memAllocatable)) < float64(2*crioMinMemLimit) {
			return true
		}
	}
	return false
}

// This test suite is used to verifies scheduler priority functions based on the default provider
var _ = SIGDescribe("SchedulerPriorities [Serial]", func() {
	var cs clientset.Interface
	var nodeList *v1.NodeList
	var systemPodsNo int
	var ns string
	f := framework.NewDefaultFramework("sched-priority")

	ginkgo.AfterEach(func() {
	})

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		ns = f.Namespace.Name
		nodeList = &v1.NodeList{}
		var err error

		e2enode.WaitForTotalHealthy(cs, time.Minute)
		nodeList, err = e2enode.GetReadySchedulableNodes(cs)
		if err != nil {
			framework.Logf("Unexpected error occurred: %v", err)
		}
		framework.ExpectNoErrorWithOffset(0, err)

		err = framework.CheckTestingNSDeletedExcept(cs, ns)
		framework.ExpectNoError(err)
		err = e2epod.WaitForPodsRunningReady(cs, metav1.NamespaceSystem, int32(systemPodsNo), 0, framework.PodReadyBeforeTimeout, map[string]string{})
		framework.ExpectNoError(err)

		// skip if the most utilized node has less than the cri-o minMemLimit available
		// otherwise we will not be able to run the test pod once all nodes are balanced
		if nodesAreTooUtilized(cs, nodeList) {
			ginkgo.Skip("nodes are too utilized to schedule test pods")
		}
	})

	ginkgo.It("Pod should be scheduled to node that don't match the PodAntiAffinity terms", func() {

		e2eskipper.SkipUnlessNodeCountIsAtLeast(2)

		ginkgo.By("Trying to launch a pod with a label to get a node which can launch it.")
		pod := runPausePod(f, pausePodConfig{
			Name:   "pod-with-label-security-s1",
			Labels: map[string]string{"security": "S1"},
		})
		nodeName := pod.Spec.NodeName

		k := v1.LabelHostname
		ginkgo.By("Verifying the node has a label " + k)
		node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		if _, hasLabel := node.Labels[k]; !hasLabel {
			// If the label is not exists, label all nodes for testing.

			ginkgo.By("Trying to apply a label on the found node.")
			k = "kubernetes.io/e2e-node-topologyKey"
			v := "topologyvalue1"
			framework.AddOrUpdateLabelOnNode(cs, nodeName, k, v)
			framework.ExpectNodeHasLabel(cs, nodeName, k, v)
			defer framework.RemoveLabelOffNode(cs, nodeName, k)

			ginkgo.By("Trying to apply a label on other nodes.")
			v = "topologyvalue2"
			for _, node := range nodeList.Items {
				if node.Name != nodeName {
					framework.AddOrUpdateLabelOnNode(cs, node.Name, k, v)
					framework.ExpectNodeHasLabel(cs, node.Name, k, v)
					defer framework.RemoveLabelOffNode(cs, node.Name, k)
				}
			}
		}

		// make the nodes have balanced cpu,mem usage
		cleanUp, err := createBalancedPodForNodes(f, cs, ns, nodeList.Items, podRequestedResource, 0.6)
		defer cleanUp()
		framework.ExpectNoError(err)
		ginkgo.By("Trying to launch the pod with podAntiAffinity.")
		labelPodName := "pod-with-pod-antiaffinity"
		pod = createPausePod(f, pausePodConfig{
			Resources: podRequestedResource,
			Name:      labelPodName,
			Affinity: &v1.Affinity{
				PodAntiAffinity: &v1.PodAntiAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
						{
							PodAffinityTerm: v1.PodAffinityTerm{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "security",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"S1", "value2"},
										},
										{
											Key:      "security",
											Operator: metav1.LabelSelectorOpNotIn,
											Values:   []string{"S2"},
										}, {
											Key:      "security",
											Operator: metav1.LabelSelectorOpExists,
										},
									},
								},
								TopologyKey: k,
								Namespaces:  []string{ns},
							},
							Weight: 10,
						},
					},
				},
			},
		})
		ginkgo.By("Wait the pod becomes running")
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(f.ClientSet, pod.Name, f.Namespace.Name))
		labelPod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), labelPodName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		ginkgo.By("Verify the pod was scheduled to the expected node.")
		framework.ExpectNotEqual(labelPod.Spec.NodeName, nodeName)
	})

	ginkgo.It("Pod should avoid nodes that have avoidPod annotation", func() {
		nodeName := nodeList.Items[0].Name
		// make the nodes have balanced cpu,mem usage
		cleanUp, err := createBalancedPodForNodes(f, cs, ns, nodeList.Items, podRequestedResource, 0.5)
		defer cleanUp()
		framework.ExpectNoError(err)
		ginkgo.By("Create a RC, with 0 replicas")
		rc := createRC(ns, "scheduler-priority-avoid-pod", int32(0), map[string]string{"name": "scheduler-priority-avoid-pod"}, f, podRequestedResource)
		// Cleanup the replication controller when we are done.
		defer func() {
			// Resize the replication controller to zero to get rid of pods.
			if err := e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, rc.Name); err != nil {
				framework.Logf("Failed to cleanup replication controller %v: %v.", rc.Name, err)
			}
		}()

		ginkgo.By("Trying to apply avoidPod annotations on the first node.")
		avoidPod := v1.AvoidPods{
			PreferAvoidPods: []v1.PreferAvoidPodsEntry{
				{
					PodSignature: v1.PodSignature{
						PodController: &metav1.OwnerReference{
							APIVersion: "v1",
							Kind:       "ReplicationController",
							Name:       rc.Name,
							UID:        rc.UID,
							Controller: func() *bool { b := true; return &b }(),
						},
					},
					Reason:  "some reson",
					Message: "some message",
				},
			},
		}
		action := func() error {
			addOrUpdateAvoidPodOnNode(cs, nodeName, avoidPod)
			return nil
		}
		predicate := func(node *v1.Node) bool {
			val, err := json.Marshal(avoidPod)
			if err != nil {
				return false
			}
			return node.Annotations[v1.PreferAvoidPodsAnnotationKey] == string(val)
		}
		success, err := observeNodeUpdateAfterAction(f.ClientSet, nodeName, predicate, action)
		framework.ExpectNoError(err)
		framework.ExpectEqual(success, true)

		defer removeAvoidPodsOffNode(cs, nodeName)

		ginkgo.By(fmt.Sprintf("Scale the RC: %s to len(nodeList.Item)-1 : %v.", rc.Name, len(nodeList.Items)-1))

		e2erc.ScaleRC(f.ClientSet, f.ScalesGetter, ns, rc.Name, uint(len(nodeList.Items)-1), true)
		testPods, err := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "name=scheduler-priority-avoid-pod",
		})
		framework.ExpectNoError(err)
		ginkgo.By(fmt.Sprintf("Verify the pods should not scheduled to the node: %s", nodeName))
		for _, pod := range testPods.Items {
			framework.ExpectNotEqual(pod.Spec.NodeName, nodeName)
		}
	})

	ginkgo.It("Pod should be preferably scheduled to nodes pod can tolerate", func() {
		// make the nodes have balanced cpu,mem usage ratio
		cleanUp, err := createBalancedPodForNodes(f, cs, ns, nodeList.Items, podRequestedResource, 0.5)
		defer cleanUp()
		framework.ExpectNoError(err)
		// Apply 10 taints to first node
		nodeName := nodeList.Items[0].Name

		// First, create a set of tolerable taints (+tolerations) for the first node.
		// Generate 10 tolerable taints for the first node (and matching tolerations)
		tolerableTaints := make([]v1.Taint, 0)
		var tolerations []v1.Toleration
		for i := 0; i < 10; i++ {
			testTaint := getRandomTaint()
			tolerableTaints = append(tolerableTaints, testTaint)
			tolerations = append(tolerations, v1.Toleration{Key: testTaint.Key, Value: testTaint.Value, Effect: testTaint.Effect})
		}
		// Generate 10 intolerable taints for each of the remaining nodes
		intolerableTaints := make(map[string][]v1.Taint)
		for i := 1; i < len(nodeList.Items); i++ {
			nodeTaints := make([]v1.Taint, 0)
			for i := 0; i < 10; i++ {
				nodeTaints = append(nodeTaints, getRandomTaint())
			}
			intolerableTaints[nodeList.Items[i].Name] = nodeTaints
		}

		// Apply the tolerable taints generated above to the first node
		ginkgo.By("Trying to apply 10 (tolerable) taints on the first node.")
		// We immediately defer the removal of these taints because addTaintToNode can
		// panic and RemoveTaintsOffNode does not return an error if the taint does not exist.
		defer e2enode.RemoveTaintsOffNode(cs, nodeName, tolerableTaints)
		for _, taint := range tolerableTaints {
			addTaintToNode(cs, nodeName, taint)
		}
		// Apply the intolerable taints to each of the following nodes
		ginkgo.By("Adding 10 intolerable taints to all other nodes")
		for i := 1; i < len(nodeList.Items); i++ {
			node := nodeList.Items[i]
			defer e2enode.RemoveTaintsOffNode(cs, node.Name, intolerableTaints[node.Name])
			for _, taint := range intolerableTaints[node.Name] {
				addTaintToNode(cs, node.Name, taint)
			}
		}

		tolerationPodName := "with-tolerations"
		ginkgo.By("Create a pod that tolerates all the taints of the first node.")
		pod := createPausePod(f, pausePodConfig{
			Name:        tolerationPodName,
			Tolerations: tolerations,
		})
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(f.ClientSet, pod.Name, f.Namespace.Name))

		ginkgo.By("Pod should prefer scheduled to the node that pod can tolerate.")
		tolePod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), tolerationPodName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		framework.ExpectEqual(tolePod.Spec.NodeName, nodeName)
	})

	ginkgo.Context("PodTopologySpread Scoring", func() {
		var nodeNames []string
		topologyKey := "kubernetes.io/e2e-pts-score"

		ginkgo.BeforeEach(func() {
			if len(nodeList.Items) < 2 {
				ginkgo.Skip("At least 2 nodes are required to run the test")
			}
			ginkgo.By("Trying to get 2 available nodes which can run pod")
			nodeNames = Get2NodesThatCanRunPod(f)
			ginkgo.By(fmt.Sprintf("Apply dedicated topologyKey %v for this test on the 2 nodes.", topologyKey))
			for _, nodeName := range nodeNames {
				framework.AddOrUpdateLabelOnNode(cs, nodeName, topologyKey, nodeName)
			}
		})
		ginkgo.AfterEach(func() {
			for _, nodeName := range nodeNames {
				framework.RemoveLabelOffNode(cs, nodeName, topologyKey)
			}
		})

		ginkgo.It("validates pod should be preferably scheduled to node which makes the matching pods more evenly distributed", func() {
			var nodes []v1.Node
			for _, nodeName := range nodeNames {
				node, err := cs.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
				framework.ExpectNoError(err)
				nodes = append(nodes, *node)
			}

			// Make the nodes have balanced cpu,mem usage.
			cleanUp, err := createBalancedPodForNodes(f, cs, ns, nodes, podRequestedResource, 0.5)
			defer cleanUp()
			framework.ExpectNoError(err)

			replicas := 4
			podLabel := "e2e-pts-score"
			ginkgo.By(fmt.Sprintf("Run a ReplicaSet with %v replicas on node %q", replicas, nodeNames[0]))
			rsConfig := pauseRSConfig{
				Replicas: int32(replicas),
				PodConfig: pausePodConfig{
					Name:         podLabel,
					Namespace:    ns,
					Labels:       map[string]string{podLabel: "foo"},
					NodeSelector: map[string]string{topologyKey: nodeNames[0]},
				},
			}
			runPauseRS(f, rsConfig)

			// Run a Pod with WhenUnsatisfiable:ScheduleAnyway.
			podCfg := pausePodConfig{
				Name:      "test-pod",
				Namespace: ns,
				// The labels shouldn't match the preceding ReplicaSet, otherwise it will
				// be claimed as orphan of the ReplicaSet.
				Labels: map[string]string{podLabel: "bar"},
				Affinity: &v1.Affinity{
					NodeAffinity: &v1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
							NodeSelectorTerms: []v1.NodeSelectorTerm{
								{
									MatchExpressions: []v1.NodeSelectorRequirement{
										{
											Key:      topologyKey,
											Operator: v1.NodeSelectorOpIn,
											Values:   nodeNames,
										},
									},
								},
							},
						},
					},
				},
				TopologySpreadConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       topologyKey,
						WhenUnsatisfiable: v1.ScheduleAnyway,
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      podLabel,
									Operator: metav1.LabelSelectorOpExists,
								},
							},
						},
					},
				},
			}
			testPod := runPausePod(f, podCfg)
			ginkgo.By(fmt.Sprintf("Verifying if the test-pod lands on node %q", nodeNames[1]))
			framework.ExpectEqual(nodeNames[1], testPod.Spec.NodeName)
		})
	})
})

// createBalancedPodForNodes creates a pod per node that asks for enough resources to make all nodes have the same mem/cpu usage ratio.
func createBalancedPodForNodes(f *framework.Framework, cs clientset.Interface, ns string, nodes []v1.Node, requestedResource *v1.ResourceRequirements, ratio float64) (func(), error) {
	cleanUp := func() {
		// Delete all remaining pods
		err := cs.CoreV1().Pods(ns).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set(balancePodLabel)).String(),
		})
		if err != nil {
			framework.Logf("Failed to delete memory balanced pods: %v.", err)
		} else {
			err := wait.PollImmediate(2*time.Second, time.Minute, func() (bool, error) {
				podList, err := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
					LabelSelector: labels.SelectorFromSet(labels.Set(balancePodLabel)).String(),
				})
				if err != nil {
					framework.Logf("Failed to list memory balanced pods: %v.", err)
					return false, nil
				}
				if len(podList.Items) > 0 {
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				framework.Logf("Failed to wait until all memory balanced pods are deleted: %v.", err)
			}
		}
	}

	// find the max, if the node has the max,use the one, if not,use the ratio parameter
	var maxCPUFraction, maxMemFraction float64 = ratio, ratio
	var cpuFractionMap = make(map[string]float64)
	var memFractionMap = make(map[string]float64)

	for _, node := range nodes {
		cpuFraction, memFraction, _, _ := computeCPUMemFraction(cs, node, requestedResource)
		cpuFractionMap[node.Name] = cpuFraction
		memFractionMap[node.Name] = memFraction
		if cpuFraction > maxCPUFraction {
			maxCPUFraction = cpuFraction
		}
		if memFraction > maxMemFraction {
			maxMemFraction = memFraction
		}
	}

	// we need the max one to keep the same cpu/mem use rate
	ratio = math.Max(maxCPUFraction, maxMemFraction)
	for _, node := range nodes {
		memAllocatable, found := node.Status.Allocatable[v1.ResourceMemory]
		framework.ExpectEqual(found, true)
		memAllocatableVal := memAllocatable.Value()

		cpuAllocatable, found := node.Status.Allocatable[v1.ResourceCPU]
		framework.ExpectEqual(found, true)
		cpuAllocatableMil := cpuAllocatable.MilliValue()

		needCreateResource := v1.ResourceList{}
		cpuFraction := cpuFractionMap[node.Name]
		memFraction := memFractionMap[node.Name]
		needCreateResource[v1.ResourceCPU] = *resource.NewMilliQuantity(int64((ratio-cpuFraction)*float64(cpuAllocatableMil)), resource.DecimalSI)

		// add crioMinMemLimit to ensure that all pods are setting at least that much for a limit, while keeping the same ratios
		needCreateResource[v1.ResourceMemory] = *resource.NewQuantity(int64((ratio-memFraction)*float64(memAllocatableVal)+float64(crioMinMemLimit)), resource.BinarySI)

		podConfig := &pausePodConfig{
			Name:   "",
			Labels: balancePodLabel,
			Resources: &v1.ResourceRequirements{
				Requests: needCreateResource,
			},
			Affinity: &v1.Affinity{
				NodeAffinity: &v1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
						NodeSelectorTerms: []v1.NodeSelectorTerm{
							{
								MatchFields: []v1.NodeSelectorRequirement{
									{Key: "metadata.name", Operator: v1.NodeSelectorOpIn, Values: []string{node.Name}},
								},
							},
						},
					},
				},
			},
		}

		err := testutils.StartPods(cs, 1, ns, string(uuid.NewUUID()),
			*initPausePod(f, *podConfig), true, framework.Logf)

		if err != nil {
			return cleanUp, err
		}
	}

	for _, node := range nodes {
		ginkgo.By("Compute Cpu, Mem Fraction after create balanced pods.")
		computeCPUMemFraction(cs, node, requestedResource)
	}

	return cleanUp, nil
}

func computeCPUMemFraction(cs clientset.Interface, node v1.Node, resource *v1.ResourceRequirements) (float64, float64, int64, int64) {
	framework.Logf("ComputeCPUMemFraction for node: %v", node.Name)
	totalRequestedCPUResource := resource.Requests.Cpu().MilliValue()
	totalRequestedMemResource := resource.Requests.Memory().Value()
	allpods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		framework.Failf("Expect error of invalid, got : %v", err)
	}
	for _, pod := range allpods.Items {
		if pod.Spec.NodeName == node.Name {
			framework.Logf("Pod for on the node: %v, Cpu: %v, Mem: %v", pod.Name, getNonZeroRequests(&pod).MilliCPU, getNonZeroRequests(&pod).Memory)
			// Ignore best effort pods while computing fractions as they won't be taken in account by scheduler.
			if v1qos.GetPodQOS(&pod) == v1.PodQOSBestEffort {
				continue
			}
			totalRequestedCPUResource += getNonZeroRequests(&pod).MilliCPU
			totalRequestedMemResource += getNonZeroRequests(&pod).Memory
		}
	}
	cpuAllocatable, found := node.Status.Allocatable[v1.ResourceCPU]
	framework.ExpectEqual(found, true)
	cpuAllocatableMil := cpuAllocatable.MilliValue()

	floatOne := float64(1)
	cpuFraction := float64(totalRequestedCPUResource) / float64(cpuAllocatableMil)
	if cpuFraction > floatOne {
		cpuFraction = floatOne
	}
	memAllocatable, found := node.Status.Allocatable[v1.ResourceMemory]
	framework.ExpectEqual(found, true)
	memAllocatableVal := memAllocatable.Value()
	memFraction := float64(totalRequestedMemResource) / float64(memAllocatableVal)
	if memFraction > floatOne {
		memFraction = floatOne
	}

	framework.Logf("Node: %v, totalRequestedCPUResource: %v, cpuAllocatableMil: %v, cpuFraction: %v", node.Name, totalRequestedCPUResource, cpuAllocatableMil, cpuFraction)
	framework.Logf("Node: %v, totalRequestedMemResource: %v, memAllocatableVal: %v, memFraction: %v", node.Name, totalRequestedMemResource, memAllocatableVal, memFraction)

	return cpuFraction, memFraction, cpuAllocatableMil, memAllocatableVal
}

func getNonZeroRequests(pod *v1.Pod) Resource {
	result := Resource{}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		cpu, memory := schedutil.GetNonzeroRequests(&container.Resources.Requests)
		result.MilliCPU += cpu
		result.Memory += memory
	}
	return result
}

func createRC(ns, rsName string, replicas int32, rcPodLabels map[string]string, f *framework.Framework, resource *v1.ResourceRequirements) *v1.ReplicationController {
	rc := &v1.ReplicationController{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ReplicationController",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: rsName,
		},
		Spec: v1.ReplicationControllerSpec{
			Replicas: &replicas,
			Template: &v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: rcPodLabels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:      rsName,
							Image:     imageutils.GetPauseImageName(),
							Resources: *resource,
						},
					},
				},
			},
		},
	}
	rc, err := f.ClientSet.CoreV1().ReplicationControllers(ns).Create(context.TODO(), rc, metav1.CreateOptions{})
	framework.ExpectNoError(err)
	return rc
}

func getRandomTaint() v1.Taint {
	return v1.Taint{
		Key:    fmt.Sprintf("kubernetes.io/e2e-scheduling-priorities-%s", string(uuid.NewUUID()[:23])),
		Value:  fmt.Sprintf("testing-taint-value-%s", string(uuid.NewUUID())),
		Effect: v1.TaintEffectPreferNoSchedule,
	}
}

func addTaintToNode(cs clientset.Interface, nodeName string, testTaint v1.Taint) {
	e2enode.AddOrUpdateTaintOnNode(cs, nodeName, testTaint)
	framework.ExpectNodeHasTaint(cs, nodeName, &testTaint)
}
