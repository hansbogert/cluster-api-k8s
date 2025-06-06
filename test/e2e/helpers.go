//go:build e2e
// +build e2e

/*
Copyright 2020 The Kubernetes Authors.

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
package e2e

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1 "github.com/canonical/cluster-api-k8s/bootstrap/api/v1beta2"
	controlplanev1 "github.com/canonical/cluster-api-k8s/controlplane/api/v1beta2"
)

// NOTE: the code in this file is largely copied from the cluster-api test framework.
// For many functions in the original framework, they assume the underlying
// controlplane is a KubeadmControlPlane, which does not fit CK8sControlPlane.
// Therefore, we need to copy the functions and modify them to fit CK8sControlPlane.
// Source: sigs.k8s.io/cluster-api/test/framework/*

const (
	retryableOperationInterval = 3 * time.Second
	retryableOperationTimeout  = 3 * time.Minute
)

// ApplyClusterTemplateAndWaitInput is the input type for ApplyClusterTemplateAndWait.
type ApplyClusterTemplateAndWaitInput struct {
	ClusterProxy                 framework.ClusterProxy
	ConfigCluster                clusterctl.ConfigClusterInput
	CNIManifestPath              string
	WaitForClusterIntervals      []interface{}
	WaitForControlPlaneIntervals []interface{}
	WaitForMachineDeployments    []interface{}
	WaitForMachinePools          []interface{}
	CreateOrUpdateOpts           []framework.CreateOrUpdateOption
	PreWaitForCluster            func()
	PostMachinesProvisioned      func()
	ControlPlaneWaiters
}

// Waiter is a function that runs and waits for a long-running operation to finish and updates the result.
type Waiter func(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult)

// ControlPlaneWaiters are Waiter functions for the control plane.
type ControlPlaneWaiters struct {
	WaitForControlPlaneInitialized   Waiter
	WaitForControlPlaneMachinesReady Waiter
}

// ApplyClusterTemplateAndWaitResult is the output type for ApplyClusterTemplateAndWait.
type ApplyClusterTemplateAndWaitResult struct {
	ClusterClass       *clusterv1.ClusterClass
	Cluster            *clusterv1.Cluster
	ControlPlane       *controlplanev1.CK8sControlPlane
	MachineDeployments []*clusterv1.MachineDeployment
	MachinePools       []*expv1.MachinePool
}

// ExpectedWorkerNodes returns the expected number of worker nodes that will
// be provisioned by the given cluster template.
func (r *ApplyClusterTemplateAndWaitResult) ExpectedWorkerNodes() int32 {
	expectedWorkerNodes := int32(0)

	for _, md := range r.MachineDeployments {
		if md.Spec.Replicas != nil {
			expectedWorkerNodes += *md.Spec.Replicas
		}
	}
	for _, mp := range r.MachinePools {
		if mp.Spec.Replicas != nil {
			expectedWorkerNodes += *mp.Spec.Replicas
		}
	}

	return expectedWorkerNodes
}

// ExpectedTotalNodes returns the expected number of nodes that will
// be provisioned by the given cluster template.
func (r *ApplyClusterTemplateAndWaitResult) ExpectedTotalNodes() int32 {
	expectedNodes := r.ExpectedWorkerNodes()

	if r.ControlPlane != nil && r.ControlPlane.Spec.Replicas != nil {
		expectedNodes += *r.ControlPlane.Spec.Replicas
	}

	return expectedNodes
}

// ApplyClusterTemplateAndWait gets a cluster template using clusterctl, and waits for the cluster to be ready.
// Important! this method assumes the cluster uses a CK8sControlPlane and MachineDeployments.
func ApplyClusterTemplateAndWait(ctx context.Context, input ApplyClusterTemplateAndWaitInput, result *ApplyClusterTemplateAndWaitResult) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for ApplyClusterTemplateAndWait")
	Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling ApplyClusterTemplateAndWait")
	Expect(result).ToNot(BeNil(), "Invalid argument. result can't be nil when calling ApplyClusterTemplateAndWait")
	Expect(input.ConfigCluster.ControlPlaneMachineCount).ToNot(BeNil())
	Expect(input.ConfigCluster.WorkerMachineCount).ToNot(BeNil())

	Byf("Creating the workload cluster with name %q using the %q template (Kubernetes %s, %d control-plane machines, %d worker machines)",
		input.ConfigCluster.ClusterName, input.ConfigCluster.Flavor, input.ConfigCluster.KubernetesVersion, *input.ConfigCluster.ControlPlaneMachineCount, *input.ConfigCluster.WorkerMachineCount)

	// Ensure we have a Cluster for dump and cleanup steps in AfterEach even if ApplyClusterTemplateAndWait fails.
	result.Cluster = &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      input.ConfigCluster.ClusterName,
			Namespace: input.ConfigCluster.Namespace,
		},
	}

	By("Getting the cluster template yaml")
	workloadClusterTemplate := clusterctl.ConfigCluster(ctx, clusterctl.ConfigClusterInput{
		// pass reference to the management cluster hosting this test
		KubeconfigPath: input.ConfigCluster.KubeconfigPath,
		// pass the clusterctl config file that points to the local provider repository created for this test,
		ClusterctlConfigPath: input.ConfigCluster.ClusterctlConfigPath,
		// select template
		Flavor: input.ConfigCluster.Flavor,
		// define template variables
		Namespace:                input.ConfigCluster.Namespace,
		ClusterName:              input.ConfigCluster.ClusterName,
		KubernetesVersion:        input.ConfigCluster.KubernetesVersion,
		ControlPlaneMachineCount: input.ConfigCluster.ControlPlaneMachineCount,
		WorkerMachineCount:       input.ConfigCluster.WorkerMachineCount,
		InfrastructureProvider:   input.ConfigCluster.InfrastructureProvider,
		// setup clusterctl logs folder
		LogFolder:           input.ConfigCluster.LogFolder,
		ClusterctlVariables: input.ConfigCluster.ClusterctlVariables,
	})
	Expect(workloadClusterTemplate).ToNot(BeNil(), "Failed to get the cluster template")

	By("Applying the cluster template yaml to the cluster")
	ApplyCustomClusterTemplateAndWait(ctx, ApplyCustomClusterTemplateAndWaitInput{
		ClusterProxy:                 input.ClusterProxy,
		CustomTemplateYAML:           workloadClusterTemplate,
		ClusterName:                  input.ConfigCluster.ClusterName,
		Namespace:                    input.ConfigCluster.Namespace,
		CNIManifestPath:              input.CNIManifestPath,
		Flavor:                       input.ConfigCluster.Flavor,
		WaitForClusterIntervals:      input.WaitForClusterIntervals,
		WaitForControlPlaneIntervals: input.WaitForControlPlaneIntervals,
		WaitForMachineDeployments:    input.WaitForMachineDeployments,
		WaitForMachinePools:          input.WaitForMachinePools,
		CreateOrUpdateOpts:           input.CreateOrUpdateOpts,
		PreWaitForCluster:            input.PreWaitForCluster,
		PostMachinesProvisioned:      input.PostMachinesProvisioned,
		ControlPlaneWaiters:          input.ControlPlaneWaiters,
	}, (*ApplyCustomClusterTemplateAndWaitResult)(result))
}

// ApplyCustomClusterTemplateAndWaitInput is the input type for ApplyCustomClusterTemplateAndWait.
type ApplyCustomClusterTemplateAndWaitInput struct {
	ClusterProxy                 framework.ClusterProxy
	CustomTemplateYAML           []byte
	ClusterName                  string
	Namespace                    string
	CNIManifestPath              string
	Flavor                       string
	WaitForClusterIntervals      []interface{}
	WaitForControlPlaneIntervals []interface{}
	WaitForMachineDeployments    []interface{}
	WaitForMachinePools          []interface{}
	CreateOrUpdateOpts           []framework.CreateOrUpdateOption
	PreWaitForCluster            func()
	PostMachinesProvisioned      func()
	ControlPlaneWaiters
}

type ApplyCustomClusterTemplateAndWaitResult struct {
	ClusterClass       *clusterv1.ClusterClass
	Cluster            *clusterv1.Cluster
	ControlPlane       *controlplanev1.CK8sControlPlane
	MachineDeployments []*clusterv1.MachineDeployment
	MachinePools       []*expv1.MachinePool
}

func ApplyCustomClusterTemplateAndWait(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult) {
	setDefaults(&input)
	Expect(ctx).NotTo(BeNil(), "ctx is required for ApplyCustomClusterTemplateAndWait")
	Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling ApplyCustomClusterTemplateAndWait")
	Expect(input.CustomTemplateYAML).NotTo(BeEmpty(), "Invalid argument. input.CustomTemplateYAML can't be empty when calling ApplyCustomClusterTemplateAndWait")
	Expect(input.ClusterName).NotTo(BeEmpty(), "Invalid argument. input.ClusterName can't be empty when calling ApplyCustomClusterTemplateAndWait")
	Expect(input.Namespace).NotTo(BeEmpty(), "Invalid argument. input.Namespace can't be empty when calling ApplyCustomClusterTemplateAndWait")
	Expect(result).ToNot(BeNil(), "Invalid argument. result can't be nil when calling ApplyClusterTemplateAndWait")

	Byf("Creating the workload cluster with name %q from the provided yaml", input.ClusterName)

	// Ensure we have a Cluster for dump and cleanup steps in AfterEach even if ApplyClusterTemplateAndWait fails.
	result.Cluster = &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      input.ClusterName,
			Namespace: input.Namespace,
		},
	}

	Byf("Applying the cluster template yaml of cluster %s", klog.KRef(input.Namespace, input.ClusterName))
	Eventually(func() error {
		return input.ClusterProxy.CreateOrUpdate(ctx, input.CustomTemplateYAML, input.CreateOrUpdateOpts...)
	}, 1*time.Minute).Should(Succeed(), "Failed to apply the cluster template")

	// Once we applied the cluster template we can run PreWaitForCluster.
	// Note: This can e.g. be used to verify the BeforeClusterCreate lifecycle hook is executed
	// and blocking correctly.
	if input.PreWaitForCluster != nil {
		Byf("Calling PreWaitForCluster for cluster %s", klog.KRef(input.Namespace, input.ClusterName))
		input.PreWaitForCluster()
	}

	Byf("Waiting for the cluster infrastructure of cluster %s to be provisioned", klog.KRef(input.Namespace, input.ClusterName))
	result.Cluster = framework.DiscoveryAndWaitForCluster(ctx, framework.DiscoveryAndWaitForClusterInput{
		Getter:    input.ClusterProxy.GetClient(),
		Namespace: input.Namespace,
		Name:      input.ClusterName,
	}, input.WaitForClusterIntervals...)

	if result.Cluster.Spec.Topology != nil {
		result.ClusterClass = framework.GetClusterClassByName(ctx, framework.GetClusterClassByNameInput{
			Getter:    input.ClusterProxy.GetClient(),
			Namespace: input.Namespace,
			Name:      result.Cluster.Spec.Topology.Class,
		})
	}

	Byf("Waiting for control plane of cluster %s to be initialized", klog.KRef(input.Namespace, input.ClusterName))
	input.WaitForControlPlaneInitialized(ctx, input, result)

	if input.CNIManifestPath != "" {
		Byf("Installing a CNI plugin to the workload cluster %s", klog.KRef(input.Namespace, input.ClusterName))
		workloadCluster := input.ClusterProxy.GetWorkloadCluster(ctx, result.Cluster.Namespace, result.Cluster.Name)

		cniYaml, err := os.ReadFile(input.CNIManifestPath)
		Expect(err).ShouldNot(HaveOccurred())

		Expect(workloadCluster.CreateOrUpdate(ctx, cniYaml)).ShouldNot(HaveOccurred())
	}

	Byf("Waiting for control plane of cluster %s to be ready", klog.KRef(input.Namespace, input.ClusterName))
	input.WaitForControlPlaneMachinesReady(ctx, input, result)

	Byf("Waiting for the machine deployments of cluster %s to be provisioned", klog.KRef(input.Namespace, input.ClusterName))
	result.MachineDeployments = framework.DiscoveryAndWaitForMachineDeployments(ctx, framework.DiscoveryAndWaitForMachineDeploymentsInput{
		Lister:  input.ClusterProxy.GetClient(),
		Cluster: result.Cluster,
	}, input.WaitForMachineDeployments...)

	Byf("Waiting for the machine pools of cluster %s to be provisioned", klog.KRef(input.Namespace, input.ClusterName))
	result.MachinePools = framework.DiscoveryAndWaitForMachinePools(ctx, framework.DiscoveryAndWaitForMachinePoolsInput{
		Getter:  input.ClusterProxy.GetClient(),
		Lister:  input.ClusterProxy.GetClient(),
		Cluster: result.Cluster,
	}, input.WaitForMachinePools...)

	if input.PostMachinesProvisioned != nil {
		Byf("Calling PostMachinesProvisioned for cluster %s", klog.KRef(input.Namespace, input.ClusterName))
		input.PostMachinesProvisioned()
	}
}

// setDefaults sets the default values for ApplyCustomClusterTemplateAndWaitInput if not set.
// Currently, we set the default ControlPlaneWaiters here, which are implemented for CK8sControlPlane.
func setDefaults(input *ApplyCustomClusterTemplateAndWaitInput) {
	if input.WaitForControlPlaneInitialized == nil {
		input.WaitForControlPlaneInitialized = func(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult) {
			result.ControlPlane = DiscoveryAndWaitForCK8sControlPlaneInitialized(ctx, DiscoveryAndWaitForCK8sControlPlaneInitializedInput{
				Lister:  input.ClusterProxy.GetClient(),
				Cluster: result.Cluster,
			}, input.WaitForControlPlaneIntervals...)
		}
	}

	if input.WaitForControlPlaneMachinesReady == nil {
		input.WaitForControlPlaneMachinesReady = func(ctx context.Context, input ApplyCustomClusterTemplateAndWaitInput, result *ApplyCustomClusterTemplateAndWaitResult) {
			WaitForControlPlaneAndMachinesReady(ctx, WaitForControlPlaneAndMachinesReadyInput{
				GetLister:    input.ClusterProxy.GetClient(),
				Cluster:      result.Cluster,
				ControlPlane: result.ControlPlane,
			}, input.WaitForControlPlaneIntervals...)
		}
	}
}

// GetCK8sControlPlaneByClusterInput is the input for GetCK8sControlPlaneByCluster.
type GetCK8sControlPlaneByClusterInput struct {
	Lister      framework.Lister
	ClusterName string
	Namespace   string
}

// GetCK8sControlPlaneByCluster returns the CK8sControlPlane objects for a cluster.
// Important! this method relies on labels that are created by the CAPI controllers during the first reconciliation, so
// it is necessary to ensure this is already happened before calling it.
func GetCK8sControlPlaneByCluster(ctx context.Context, input GetCK8sControlPlaneByClusterInput) *controlplanev1.CK8sControlPlane {
	controlPlaneList := &controlplanev1.CK8sControlPlaneList{}
	Eventually(func() error {
		return input.Lister.List(ctx, controlPlaneList, byClusterOptions(input.ClusterName, input.Namespace)...)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Failed to list CK8sControlPlane object for Cluster %s", klog.KRef(input.Namespace, input.ClusterName))
	Expect(len(controlPlaneList.Items)).ToNot(BeNumerically(">", 1), "Cluster %s should not have more than 1 CK8sControlPlane object", klog.KRef(input.Namespace, input.ClusterName))
	if len(controlPlaneList.Items) == 1 {
		return &controlPlaneList.Items[0]
	}
	return nil
}

// WaitForCK8sControlPlaneMachinesToExistInput is the input for WaitForCK8sControlPlaneMachinesToExist.
type WaitForCK8sControlPlaneMachinesToExistInput struct {
	Lister       framework.Lister
	Cluster      *clusterv1.Cluster
	ControlPlane *controlplanev1.CK8sControlPlane
}

// WaitForCK8sControlPlaneMachinesToExist will wait until all control plane machines have node refs.
func WaitForCK8sControlPlaneMachinesToExist(ctx context.Context, input WaitForCK8sControlPlaneMachinesToExistInput, intervals ...interface{}) {
	By("Waiting for all control plane nodes to exist")
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	// ControlPlane labels
	matchClusterListOption := client.MatchingLabels{
		clusterv1.MachineControlPlaneLabel: "",
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
	}

	Eventually(func() (int, error) {
		machineList := &clusterv1.MachineList{}
		if err := input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption); err != nil {
			Byf("Failed to list the machines: %+v", err)
			return 0, err
		}
		count := 0
		for _, machine := range machineList.Items {
			if machine.Status.NodeRef != nil {
				count++
			}
		}
		return count, nil
	}, intervals...).Should(Equal(int(*input.ControlPlane.Spec.Replicas)), "Timed out waiting for %d control plane machines to exist", int(*input.ControlPlane.Spec.Replicas))
}

// WaitForOneCK8sControlPlaneMachineToExistInput is the input for WaitForCK8sControlPlaneMachinesToExist.
type WaitForOneCK8sControlPlaneMachineToExistInput struct {
	Lister       framework.Lister
	Cluster      *clusterv1.Cluster
	ControlPlane *controlplanev1.CK8sControlPlane
}

// WaitForOneCK8sControlPlaneMachineToExist will wait until all control plane machines have node refs.
func WaitForOneCK8sControlPlaneMachineToExist(ctx context.Context, input WaitForOneCK8sControlPlaneMachineToExistInput, intervals ...interface{}) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for WaitForOneCK8sControlPlaneMachineToExist")
	Expect(input.Lister).ToNot(BeNil(), "Invalid argument. input.Getter can't be nil when calling WaitForOneCK8sControlPlaneMachineToExist")
	Expect(input.ControlPlane).ToNot(BeNil(), "Invalid argument. input.ControlPlane can't be nil when calling WaitForOneCK8sControlPlaneMachineToExist")

	By("Waiting for one control plane node to exist")
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	// ControlPlane labels
	matchClusterListOption := client.MatchingLabels{
		clusterv1.MachineControlPlaneLabel: "",
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
	}

	Eventually(func() (bool, error) {
		machineList := &clusterv1.MachineList{}
		if err := input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption); err != nil {
			Byf("Failed to list the machines: %+v", err)
			return false, err
		}
		count := 0
		for _, machine := range machineList.Items {
			if machine.Status.NodeRef != nil {
				count++
			}
		}
		return count > 0, nil
	}, intervals...).Should(BeTrue(), "No Control Plane machines came into existence. ")
}

// WaitForControlPlaneToBeReadyInput is the input for WaitForControlPlaneToBeReady.
type WaitForControlPlaneToBeReadyInput struct {
	Getter       framework.Getter
	ControlPlane *controlplanev1.CK8sControlPlane
}

// WaitForControlPlaneToBeReady will wait for a control plane to be ready.
func WaitForControlPlaneToBeReady(ctx context.Context, input WaitForControlPlaneToBeReadyInput, intervals ...interface{}) {
	By("Waiting for the control plane to be ready")
	controlplane := &controlplanev1.CK8sControlPlane{}
	Eventually(func() (bool, error) {
		key := client.ObjectKey{
			Namespace: input.ControlPlane.GetNamespace(),
			Name:      input.ControlPlane.GetName(),
		}
		Byf("Getting the control plane %s", klog.KObj(input.ControlPlane))
		if err := input.Getter.Get(ctx, key, controlplane); err != nil {
			return false, fmt.Errorf("failed to get KCP: %w", err)
		}

		desiredReplicas := controlplane.Spec.Replicas
		statusReplicas := controlplane.Status.Replicas
		updatedReplicas := controlplane.Status.UpdatedReplicas
		readyReplicas := controlplane.Status.ReadyReplicas
		unavailableReplicas := controlplane.Status.UnavailableReplicas

		// Control plane is still rolling out (and thus not ready) if:
		// * .spec.replicas, .status.replicas, .status.updatedReplicas,
		//   .status.readyReplicas are not equal and
		// * unavailableReplicas > 0
		Byf("Control plane %s: desired=%d, status=%d, updated=%d, ready=%d, unavailable=%d", klog.KObj(controlplane), *desiredReplicas, statusReplicas, updatedReplicas, readyReplicas, unavailableReplicas)
		if statusReplicas != *desiredReplicas ||
			updatedReplicas != *desiredReplicas ||
			readyReplicas != *desiredReplicas ||
			unavailableReplicas > 0 {
			return false, nil
		}

		return true, nil
	}, intervals...).Should(BeTrue(), framework.PrettyPrint(controlplane)+"\n")
}

// AssertControlPlaneFailureDomainsInput is the input for AssertControlPlaneFailureDomains.
type AssertControlPlaneFailureDomainsInput struct {
	Lister  framework.Lister
	Cluster *clusterv1.Cluster
}

// AssertControlPlaneFailureDomains will look at all control plane machines and see what failure domains they were
// placed in. If machines were placed in unexpected or wrong failure domains the expectation will fail.
func AssertControlPlaneFailureDomains(ctx context.Context, input AssertControlPlaneFailureDomainsInput) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for AssertControlPlaneFailureDomains")
	Expect(input.Lister).ToNot(BeNil(), "Invalid argument. input.Lister can't be nil when calling AssertControlPlaneFailureDomains")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling AssertControlPlaneFailureDomains")

	By("Checking all the control plane machines are in the expected failure domains")
	controlPlaneFailureDomains := sets.Set[string]{}
	for fd, fdSettings := range input.Cluster.Status.FailureDomains {
		if fdSettings.ControlPlane {
			controlPlaneFailureDomains.Insert(fd)
		}
	}

	// Look up all the control plane machines.
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	matchClusterListOption := client.MatchingLabels{
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}

	machineList := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list control-plane machines for the cluster %q", input.Cluster.Name)

	for _, machine := range machineList.Items {
		if machine.Spec.FailureDomain != nil {
			machineFD := *machine.Spec.FailureDomain
			if !controlPlaneFailureDomains.Has(machineFD) {
				Fail(fmt.Sprintf("Machine %s is in the %q failure domain, expecting one of the failure domain defined at cluster level", machine.Name, machineFD))
			}
		}
	}
}

// DiscoveryAndWaitForCK8sControlPlaneInitializedInput is the input type for DiscoveryAndWaitForControlPlaneInitialized.
type DiscoveryAndWaitForCK8sControlPlaneInitializedInput struct {
	Lister  framework.Lister
	Cluster *clusterv1.Cluster
}

// DiscoveryAndWaitForCK8sControlPlaneInitialized discovers the CK8sControlPlane object attached to a cluster and waits for it to be initialized.
func DiscoveryAndWaitForCK8sControlPlaneInitialized(ctx context.Context, input DiscoveryAndWaitForCK8sControlPlaneInitializedInput, intervals ...interface{}) *controlplanev1.CK8sControlPlane {
	Expect(ctx).NotTo(BeNil(), "ctx is required for DiscoveryAndWaitForControlPlaneInitialized")
	Expect(input.Lister).ToNot(BeNil(), "Invalid argument. input.Lister can't be nil when calling DiscoveryAndWaitForControlPlaneInitialized")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling DiscoveryAndWaitForControlPlaneInitialized")

	var controlPlane *controlplanev1.CK8sControlPlane
	Eventually(func(g Gomega) {
		controlPlane = GetCK8sControlPlaneByCluster(ctx, GetCK8sControlPlaneByClusterInput{
			Lister:      input.Lister,
			ClusterName: input.Cluster.Name,
			Namespace:   input.Cluster.Namespace,
		})
		g.Expect(controlPlane).ToNot(BeNil())
	}, "10s", "1s").Should(Succeed(), "Couldn't get the control plane for the cluster %s", klog.KObj(input.Cluster))

	Byf("Waiting for the first control plane machine managed by %s to be provisioned", klog.KObj(controlPlane))
	WaitForOneCK8sControlPlaneMachineToExist(ctx, WaitForOneCK8sControlPlaneMachineToExistInput{
		Lister:       input.Lister,
		Cluster:      input.Cluster,
		ControlPlane: controlPlane,
	}, intervals...)

	return controlPlane
}

// WaitForControlPlaneAndMachinesReadyInput is the input type for WaitForControlPlaneAndMachinesReady.
type WaitForControlPlaneAndMachinesReadyInput struct {
	GetLister    framework.GetLister
	Cluster      *clusterv1.Cluster
	ControlPlane *controlplanev1.CK8sControlPlane
}

// WaitForControlPlaneAndMachinesReady waits for a CK8sControlPlane object to be ready (all the machine provisioned and one node ready).
func WaitForControlPlaneAndMachinesReady(ctx context.Context, input WaitForControlPlaneAndMachinesReadyInput, intervals ...interface{}) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for WaitForControlPlaneReady")
	Expect(input.GetLister).ToNot(BeNil(), "Invalid argument. input.GetLister can't be nil when calling WaitForControlPlaneReady")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling WaitForControlPlaneReady")
	Expect(input.ControlPlane).ToNot(BeNil(), "Invalid argument. input.ControlPlane can't be nil when calling WaitForControlPlaneReady")

	if input.ControlPlane.Spec.Replicas != nil && int(*input.ControlPlane.Spec.Replicas) > 1 {
		Byf("Waiting for the remaining control plane machines managed by %s to be provisioned", klog.KObj(input.ControlPlane))
		WaitForCK8sControlPlaneMachinesToExist(ctx, WaitForCK8sControlPlaneMachinesToExistInput{
			Lister:       input.GetLister,
			Cluster:      input.Cluster,
			ControlPlane: input.ControlPlane,
		}, intervals...)
	}

	Byf("Waiting for control plane %s to be ready (implies underlying nodes to be ready as well)", klog.KObj(input.ControlPlane))
	waitForControlPlaneToBeReadyInput := WaitForControlPlaneToBeReadyInput{
		Getter:       input.GetLister,
		ControlPlane: input.ControlPlane,
	}
	WaitForControlPlaneToBeReady(ctx, waitForControlPlaneToBeReadyInput, intervals...)

	AssertControlPlaneFailureDomains(ctx, AssertControlPlaneFailureDomainsInput{
		Lister:  input.GetLister,
		Cluster: input.Cluster,
	})
}

type ApplyCertificateRefreshAndWaitInput struct {
	Getter                  framework.Getter
	Machine                 *clusterv1.Machine
	ClusterProxy            framework.ClusterProxy
	TTL                     string
	WaitForRefreshIntervals []interface{}
}

func ApplyCertificateRefreshAndWait(ctx context.Context, input ApplyCertificateRefreshAndWaitInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.Machine).ToNot(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.TTL).ToNot(BeEmpty())

	mgmtClient := input.ClusterProxy.GetClient()

	patchHelper, err := patch.NewHelper(input.Machine, mgmtClient)
	Expect(err).ToNot(HaveOccurred())

	mAnnotations := input.Machine.GetAnnotations()
	if mAnnotations == nil {
		mAnnotations = map[string]string{}
	}

	mAnnotations[bootstrapv1.CertificatesRefreshAnnotation] = input.TTL
	input.Machine.SetAnnotations(mAnnotations)
	err = patchHelper.Patch(ctx, input.Machine)
	Expect(err).ToNot(HaveOccurred())

	By("Waiting for certificates to be refreshed")
	Eventually(func() (bool, error) {
		machine := &clusterv1.Machine{}
		if err := input.Getter.Get(ctx, client.ObjectKey{
			Namespace: input.Machine.Namespace,
			Name:      input.Machine.Name,
		}, machine); err != nil {
			return false, err
		}

		mAnnotations := machine.GetAnnotations()
		if mAnnotations == nil {
			return false, nil
		}

		status, ok := mAnnotations[bootstrapv1.CertificatesRefreshStatusAnnotation]
		if !ok {
			return false, nil
		}

		if status == bootstrapv1.CertificatesRefreshFailedStatus {
			return false, fmt.Errorf("certificates refresh failed for machine %s", machine.Name)
		}

		return status == bootstrapv1.CertificatesRefreshDoneStatus, nil
	}, input.WaitForRefreshIntervals...).Should(BeTrue(), "Certificates refresh failed for %s", input.Machine.Name)
}

type ApplyCertificateRefreshForControlPlaneInput struct {
	Lister                  framework.Lister
	Getter                  framework.Getter
	ClusterProxy            framework.ClusterProxy
	Cluster                 *clusterv1.Cluster
	TTL                     string
	WaitForRefreshIntervals []interface{}
}

func ApplyCertificateRefreshForControlPlane(ctx context.Context, input ApplyCertificateRefreshForControlPlaneInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.Cluster).ToNot(BeNil())
	Expect(input.TTL).ToNot(BeEmpty())

	By("Looking up control plane machines")
	machineList := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, machineList,
			client.InNamespace(input.Cluster.Namespace),
			client.MatchingLabels{
				clusterv1.ClusterNameLabel:         input.Cluster.Name,
				clusterv1.MachineControlPlaneLabel: "",
			})
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(),
		"Failed to list control plane machines for cluster %q", input.Cluster.Name)

	for i := range machineList.Items {
		machine := &machineList.Items[i]
		By(fmt.Sprintf("Refreshing certificates for control plane machine: %s", machine.Name))
		ApplyCertificateRefreshAndWait(ctx, ApplyCertificateRefreshAndWaitInput{
			Getter:                  input.Getter,
			Machine:                 machine,
			ClusterProxy:            input.ClusterProxy,
			TTL:                     input.TTL,
			WaitForRefreshIntervals: input.WaitForRefreshIntervals,
		})
	}
}

type ApplyCertificateRefreshForWorkerInput struct {
	Lister                  framework.Lister
	Getter                  framework.Getter
	ClusterProxy            framework.ClusterProxy
	Cluster                 *clusterv1.Cluster
	MachineDeployments      []*clusterv1.MachineDeployment
	TTL                     string
	WaitForRefreshIntervals []interface{}
}

func ApplyCertificateRefreshForWorker(ctx context.Context, input ApplyCertificateRefreshForWorkerInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.Cluster).ToNot(BeNil())
	Expect(input.MachineDeployments).ToNot(BeNil())
	Expect(input.TTL).ToNot(BeEmpty())

	for _, md := range input.MachineDeployments {
		By(fmt.Sprintf("Refreshing certificates for machines in deployment %s", md.Name))

		inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
		matchClusterListOption := client.MatchingLabels{
			clusterv1.ClusterNameLabel:           input.Cluster.Name,
			clusterv1.MachineDeploymentNameLabel: md.Name,
		}

		machineList := &clusterv1.MachineList{}
		Eventually(func() error {
			return input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption)
		}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list machines for deployment %q in the cluster %q", md.Name, input.Cluster.Name)

		for i := range machineList.Items {
			machine := &machineList.Items[i]
			By(fmt.Sprintf("Refreshing certificates for worker machine: %s", machine.Name))
			ApplyCertificateRefreshAndWait(ctx, ApplyCertificateRefreshAndWaitInput{
				Getter:                  input.Getter,
				Machine:                 machine,
				ClusterProxy:            input.ClusterProxy,
				TTL:                     input.TTL,
				WaitForRefreshIntervals: input.WaitForRefreshIntervals,
			})
		}
	}
}

type ApplyInPlaceUpgradeAndWaitInput struct {
	Getter framework.Getter
	Obj    client.Object
	// DestinationObj is used as a destination to decode whatever is retrieved from the client.
	// e.g:
	// 	{DestinationObj: &clusterv1.Machine{}, ...}
	// 	client.Get(ctx, objKey, DestinationObj)
	DestinationObj          client.Object
	ClusterProxy            framework.ClusterProxy
	UpgradeOption           string
	WaitForUpgradeIntervals []interface{}
}

// ApplyInPlaceUpgradeAndWait applies an in-place upgrade to an object and waits for the upgrade to complete.
func ApplyInPlaceUpgradeAndWait(ctx context.Context, input ApplyInPlaceUpgradeAndWaitInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.Obj).ToNot(BeNil())
	Expect(input.DestinationObj).ToNot(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.UpgradeOption).ToNot(BeEmpty())

	mgmtClient := input.ClusterProxy.GetClient()

	patchHelper, err := patch.NewHelper(input.Obj, mgmtClient)
	Expect(err).ToNot(HaveOccurred())
	annotations := input.Obj.GetAnnotations()

	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[bootstrapv1.InPlaceUpgradeToAnnotation] = input.UpgradeOption
	input.Obj.SetAnnotations(annotations)
	err = patchHelper.Patch(ctx, input.Obj)
	Expect(err).ToNot(HaveOccurred())

	By("Checking for in-place upgrade status to be equal to done")

	Eventually(func() (bool, error) {
		if err := input.Getter.Get(ctx, client.ObjectKeyFromObject(input.Obj), input.DestinationObj); err != nil {
			Byf("Failed to get the object: %+v", err)
			return false, err
		}

		mAnnotations := input.DestinationObj.GetAnnotations()

		status, ok := mAnnotations[bootstrapv1.InPlaceUpgradeStatusAnnotation]
		if !ok {
			return false, nil
		}

		return status == bootstrapv1.InPlaceUpgradeDoneStatus, nil
	}, input.WaitForUpgradeIntervals...).Should(BeTrue(), "In-place upgrade failed for %s", input.Obj.GetName())
}

type ApplyInPlaceUpgradeForControlPlaneInput struct {
	Lister                  framework.Lister
	Getter                  framework.Getter
	ClusterProxy            framework.ClusterProxy
	Cluster                 *clusterv1.Cluster
	UpgradeOption           string
	WaitForUpgradeIntervals []interface{}
}

func ApplyInPlaceUpgradeForControlPlane(ctx context.Context, input ApplyInPlaceUpgradeForControlPlaneInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.Cluster).ToNot(BeNil())
	Expect(input.UpgradeOption).ToNot(BeEmpty())

	// Look up all the control plane machines.
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	matchClusterListOption := client.MatchingLabels{
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}

	machineList := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list control-plane machines for the cluster %q", input.Cluster.Name)

	for _, machine := range machineList.Items {
		ApplyInPlaceUpgradeAndWait(ctx, ApplyInPlaceUpgradeAndWaitInput{
			Getter:                  input.Getter,
			Obj:                     &machine,
			DestinationObj:          &clusterv1.Machine{},
			ClusterProxy:            input.ClusterProxy,
			UpgradeOption:           input.UpgradeOption,
			WaitForUpgradeIntervals: input.WaitForUpgradeIntervals,
		})
	}
}

type ApplyInPlaceUpgradeForWorkerInput struct {
	Lister                  framework.Lister
	Getter                  framework.Getter
	ClusterProxy            framework.ClusterProxy
	Cluster                 *clusterv1.Cluster
	MachineDeployments      []*clusterv1.MachineDeployment
	UpgradeOption           string
	WaitForUpgradeIntervals []interface{}
}

func ApplyInPlaceUpgradeForWorker(ctx context.Context, input ApplyInPlaceUpgradeForWorkerInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.Cluster).ToNot(BeNil())
	Expect(input.MachineDeployments).ToNot(BeNil())
	Expect(input.UpgradeOption).ToNot(BeEmpty())

	for _, md := range input.MachineDeployments {
		// Look up all the worker machines.
		inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
		matchClusterListOption := client.MatchingLabels{
			clusterv1.ClusterNameLabel:           input.Cluster.Name,
			clusterv1.MachineDeploymentNameLabel: md.Name,
		}

		machineList := &clusterv1.MachineList{}
		Eventually(func() error {
			return input.Lister.List(ctx, machineList, inClustersNamespaceListOption, matchClusterListOption)
		}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list worker machines for the cluster %q", input.Cluster.Name)

		for _, machine := range machineList.Items {
			ApplyInPlaceUpgradeAndWait(ctx, ApplyInPlaceUpgradeAndWaitInput{
				Getter:                  input.Getter,
				Obj:                     &machine,
				DestinationObj:          &clusterv1.Machine{},
				ClusterProxy:            input.ClusterProxy,
				UpgradeOption:           input.UpgradeOption,
				WaitForUpgradeIntervals: input.WaitForUpgradeIntervals,
			})
		}
	}
}

type ApplyInPlaceUpgradeForMachineDeploymentInput struct {
	Lister                  framework.Lister
	Getter                  framework.Getter
	ClusterProxy            framework.ClusterProxy
	Cluster                 *clusterv1.Cluster
	MachineDeployments      []*clusterv1.MachineDeployment
	UpgradeOption           string
	WaitForUpgradeIntervals []interface{}
}

func ApplyInPlaceUpgradeForMachineDeployment(ctx context.Context, input ApplyInPlaceUpgradeForMachineDeploymentInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.Cluster).ToNot(BeNil())
	Expect(input.MachineDeployments).ToNot(BeNil())
	Expect(input.UpgradeOption).ToNot(BeEmpty())

	var machineDeployment *clusterv1.MachineDeployment
	for _, md := range input.MachineDeployments {
		if md.Labels[clusterv1.ClusterNameLabel] == input.Cluster.Name {
			machineDeployment = md
			break
		}
	}
	Expect(machineDeployment).ToNot(BeNil())

	ApplyInPlaceUpgradeAndWait(ctx, ApplyInPlaceUpgradeAndWaitInput{
		Getter:                  input.Getter,
		Obj:                     machineDeployment,
		DestinationObj:          &clusterv1.MachineDeployment{},
		ClusterProxy:            input.ClusterProxy,
		UpgradeOption:           input.UpgradeOption,
		WaitForUpgradeIntervals: input.WaitForUpgradeIntervals,
	})

	// Make sure all the machines are upgraded
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	belongsToMDListOption := client.MatchingLabels{
		clusterv1.ClusterNameLabel:           input.Cluster.Name,
		clusterv1.MachineDeploymentNameLabel: machineDeployment.Name,
	}

	mdMachineList := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, mdMachineList, inClustersNamespaceListOption, belongsToMDListOption)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list machines for the machineDeployment %q", machineDeployment.Name)

	for _, machine := range mdMachineList.Items {
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeStatusAnnotation]).To(Equal(bootstrapv1.InPlaceUpgradeDoneStatus))
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeReleaseAnnotation]).To(Equal(input.UpgradeOption))
	}

	// Make sure other machines are not upgraded
	allMachines := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, allMachines, inClustersNamespaceListOption)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list all machines")

	for _, machine := range allMachines.Items {
		// skip the ones belong to the MD under test machines
		if isMachineInList(machine, mdMachineList) {
			continue
		}

		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeToAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeLastFailedAttemptAtAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeChangeIDAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeStatusAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeReleaseAnnotation]).To(BeEmpty())
	}
}

type ApplyInPlaceUpgradeForCK8sControlPlaneInput struct {
	Lister                  framework.Lister
	Getter                  framework.Getter
	ClusterProxy            framework.ClusterProxy
	Cluster                 *clusterv1.Cluster
	UpgradeOption           string
	WaitForUpgradeIntervals []interface{}
}

func ApplyInPlaceUpgradeForCK8sControlPlane(ctx context.Context, input ApplyInPlaceUpgradeForCK8sControlPlaneInput) {
	Expect(ctx).NotTo(BeNil())
	Expect(input.ClusterProxy).ToNot(BeNil())
	Expect(input.Cluster).ToNot(BeNil())
	Expect(input.UpgradeOption).ToNot(BeEmpty())

	ck8sCP := GetCK8sControlPlaneByCluster(ctx, GetCK8sControlPlaneByClusterInput{
		Lister:      input.Lister,
		ClusterName: input.Cluster.Name,
		Namespace:   input.Cluster.Namespace,
	})
	Expect(ck8sCP).ToNot(BeNil())

	ApplyInPlaceUpgradeAndWait(ctx, ApplyInPlaceUpgradeAndWaitInput{
		Getter:                  input.Getter,
		Obj:                     ck8sCP,
		DestinationObj:          &controlplanev1.CK8sControlPlane{},
		ClusterProxy:            input.ClusterProxy,
		UpgradeOption:           input.UpgradeOption,
		WaitForUpgradeIntervals: input.WaitForUpgradeIntervals,
	})

	// Make sure all the machines are upgraded
	inClustersNamespaceListOption := client.InNamespace(input.Cluster.Namespace)
	cpMatchLabelsListOption := client.MatchingLabels{
		clusterv1.ClusterNameLabel:         input.Cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}

	cpMachineList := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, cpMachineList, inClustersNamespaceListOption, cpMatchLabelsListOption)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list machines for the CK8sControlPlane %q", ck8sCP.Name)

	for _, machine := range cpMachineList.Items {
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeStatusAnnotation]).To(Equal(bootstrapv1.InPlaceUpgradeDoneStatus))
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeReleaseAnnotation]).To(Equal(input.UpgradeOption))
	}

	// Make sure other machines (non-cp ones) are not upgraded
	allMachines := &clusterv1.MachineList{}
	Eventually(func() error {
		return input.Lister.List(ctx, allMachines, inClustersNamespaceListOption)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Couldn't list all machines")

	for _, machine := range allMachines.Items {
		// skip the control plane machines
		if isMachineInList(machine, cpMachineList) {
			continue
		}

		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeToAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeLastFailedAttemptAtAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeChangeIDAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeStatusAnnotation]).To(BeEmpty())
		Expect(machine.Annotations[bootstrapv1.InPlaceUpgradeReleaseAnnotation]).To(BeEmpty())
	}
}

// UpgradeControlPlaneAndWaitForUpgradeInput is the input type for UpgradeControlPlaneAndWaitForUpgrade.
type UpgradeControlPlaneAndWaitForUpgradeInput struct {
	ClusterProxy                framework.ClusterProxy
	Cluster                     *clusterv1.Cluster
	ControlPlane                *controlplanev1.CK8sControlPlane
	MaxControlPlaneMachineCount int64
	KubernetesUpgradeVersion    string
	UpgradeMachineTemplate      *string
	WaitForMachinesToBeUpgraded []interface{}
}

// UpgradeControlPlaneAndWaitForUpgrade upgrades a KubeadmControlPlane and waits for it to be upgraded.
func UpgradeControlPlaneAndWaitForUpgrade(ctx context.Context, input UpgradeControlPlaneAndWaitForUpgradeInput) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for UpgradeControlPlaneAndWaitForUpgrade")
	Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling UpgradeControlPlaneAndWaitForUpgrade")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling UpgradeControlPlaneAndWaitForUpgrade")
	Expect(input.ControlPlane).ToNot(BeNil(), "Invalid argument. input.ControlPlane can't be nil when calling UpgradeControlPlaneAndWaitForUpgrade")
	Expect(input.KubernetesUpgradeVersion).ToNot(BeNil(), "Invalid argument. input.KubernetesUpgradeVersion can't be empty when calling UpgradeControlPlaneAndWaitForUpgrade")

	mgmtClient := input.ClusterProxy.GetClient()

	Byf("Patching the new kubernetes version to KCP")
	patchHelper, err := patch.NewHelper(input.ControlPlane, mgmtClient)
	Expect(err).ToNot(HaveOccurred())

	input.ControlPlane.Spec.Version = input.KubernetesUpgradeVersion

	// Create a new ObjectReference for the infrastructure provider
	newInfrastructureRef := corev1.ObjectReference{
		APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
		Kind:       "DockerMachineTemplate",
		Name:       fmt.Sprintf("%s-control-plane-new", input.Cluster.Name),
		Namespace:  input.ControlPlane.Spec.MachineTemplate.InfrastructureRef.Namespace,
	}

	// Update the infrastructureRef
	input.ControlPlane.Spec.MachineTemplate.InfrastructureRef = newInfrastructureRef

	Eventually(func() error {
		return patchHelper.Patch(ctx, input.ControlPlane)
	}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Failed to patch the new kubernetes version to KCP %s", klog.KObj(input.ControlPlane))

	Byf("Waiting for control-plane machines to have the upgraded kubernetes version")
	WaitForControlPlaneMachinesToBeUpgraded(ctx, WaitForControlPlaneMachinesToBeUpgradedInput{
		Lister:                   mgmtClient,
		Cluster:                  input.Cluster,
		MachineCount:             int(*input.ControlPlane.Spec.Replicas),
		MaxMachineCount:          int(input.MaxControlPlaneMachineCount),
		KubernetesUpgradeVersion: input.KubernetesUpgradeVersion,
	}, input.WaitForMachinesToBeUpgraded...)
}

// WaitForControlPlaneMachinesToBeUpgradedInput is the input for WaitForControlPlaneMachinesToBeUpgraded.
// originally from: https://github.com/kubernetes-sigs/cluster-api/blob/cee1200faf24a618bcf44707e7d63eb8f69c19e0/test/framework/machine_helpers.go#L147
// Changes: Added MaxMachineCount field.
type WaitForControlPlaneMachinesToBeUpgradedInput struct {
	Lister                   framework.Lister
	Cluster                  *clusterv1.Cluster
	KubernetesUpgradeVersion string
	MachineCount             int
	MaxMachineCount          int
}

// WaitForControlPlaneMachinesToBeUpgraded waits until all machines are upgraded to the correct Kubernetes version.
// originally from: https://github.com/kubernetes-sigs/cluster-api/blob/cee1200faf24a618bcf44707e7d63eb8f69c19e0/test/framework/machine_helpers.go#L155
// Changes: Handles MaxMachineCount.
func WaitForControlPlaneMachinesToBeUpgraded(ctx context.Context, input WaitForControlPlaneMachinesToBeUpgradedInput, intervals ...interface{}) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for WaitForControlPlaneMachinesToBeUpgraded")
	Expect(input.Lister).ToNot(BeNil(), "Invalid argument. input.Lister can't be nil when calling WaitForControlPlaneMachinesToBeUpgraded")
	Expect(input.KubernetesUpgradeVersion).ToNot(BeEmpty(), "Invalid argument. input.KubernetesUpgradeVersion can't be empty when calling WaitForControlPlaneMachinesToBeUpgraded")
	Expect(input.MachineCount).To(BeNumerically(">", 0), "Invalid argument. input.MachineCount can't be smaller than 1 when calling WaitForControlPlaneMachinesToBeUpgraded")
	Expect(input.MaxMachineCount).To(BeNumerically(">", 0), "Invalid argument. input.MaxMachineCount can't be smaller than 1 when calling WaitForControlPlaneMachinesToBeUpgraded")

	Byf("Ensuring all control-plane machines have upgraded kubernetes version %s", input.KubernetesUpgradeVersion)

	Eventually(func() (int, error) {
		machines := framework.GetControlPlaneMachinesByCluster(ctx, framework.GetControlPlaneMachinesByClusterInput{
			Lister:      input.Lister,
			ClusterName: input.Cluster.Name,
			Namespace:   input.Cluster.Namespace,
		})

		if len(machines) > input.MaxMachineCount {
			return -1, StopTrying(fmt.Sprintf("more Machines than expected (%d) are present", input.MaxMachineCount))
		}

		upgraded := 0
		for _, machine := range machines {
			m := machine
			if *m.Spec.Version == input.KubernetesUpgradeVersion && conditions.IsTrue(&m, clusterv1.MachineNodeHealthyCondition) {
				upgraded++
			}
		}
		if len(machines) > upgraded {
			return 0, errors.New("old Machines remain")
		}
		return upgraded, nil
	}, intervals...).Should(Equal(input.MachineCount), "Timed out waiting for all control-plane machines in Cluster %s to be upgraded to kubernetes version %s", klog.KObj(input.Cluster), input.KubernetesUpgradeVersion)
}

// UpgradeMachineDeploymentsAndWait upgrades a machine deployment and waits for its machines to be upgraded.
func UpgradeMachineDeploymentsAndWait(ctx context.Context, input framework.UpgradeMachineDeploymentsAndWaitInput) {
	Expect(ctx).NotTo(BeNil(), "ctx is required for UpgradeMachineDeploymentsAndWait")
	Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling UpgradeMachineDeploymentsAndWait")
	Expect(input.Cluster).ToNot(BeNil(), "Invalid argument. input.Cluster can't be nil when calling UpgradeMachineDeploymentsAndWait")
	Expect(input.UpgradeVersion).ToNot(BeNil(), "Invalid argument. input.UpgradeVersion can't be nil when calling UpgradeMachineDeploymentsAndWait")
	Expect(input.MachineDeployments).ToNot(BeEmpty(), "Invalid argument. input.MachineDeployments can't be empty when calling UpgradeMachineDeploymentsAndWait")

	mgmtClient := input.ClusterProxy.GetClient()

	for _, deployment := range input.MachineDeployments {
		patchHelper, err := patch.NewHelper(deployment, mgmtClient)
		Expect(err).ToNot(HaveOccurred())

		oldVersion := deployment.Spec.Template.Spec.Version
		deployment.Spec.Template.Spec.Version = &input.UpgradeVersion
		// Create a new ObjectReference for the infrastructure provider
		newInfrastructureRef := corev1.ObjectReference{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "DockerMachineTemplate",
			Name:       fmt.Sprintf("%s-md-new-0", input.Cluster.Name),
			Namespace:  deployment.Spec.Template.Spec.InfrastructureRef.Namespace,
		}

		// Update the infrastructureRef
		deployment.Spec.Template.Spec.InfrastructureRef = newInfrastructureRef
		Eventually(func() error {
			return patchHelper.Patch(ctx, deployment)
		}, retryableOperationTimeout, retryableOperationInterval).Should(Succeed(), "Failed to patch Kubernetes version on MachineDeployment %s", klog.KObj(deployment))

		Byf("Waiting for Kubernetes versions of machines in MachineDeployment %s to be upgraded from %s to %s",
			klog.KObj(deployment), *oldVersion, input.UpgradeVersion)
		framework.WaitForMachineDeploymentMachinesToBeUpgraded(ctx, framework.WaitForMachineDeploymentMachinesToBeUpgradedInput{
			Lister:                   mgmtClient,
			Cluster:                  input.Cluster,
			MachineCount:             int(*deployment.Spec.Replicas),
			KubernetesUpgradeVersion: input.UpgradeVersion,
			MachineDeployment:        *deployment,
		}, input.WaitForMachinesToBeUpgraded...)
	}
}

type WaitForNodesReadyInput struct {
	Lister            framework.Lister
	KubernetesVersion string
	Count             int
	WaitForNodesReady []interface{}
}

// WaitForNodesReady waits until there are exactly the given count nodes and they have the correct Kubernetes minor version
// and are ready.
func WaitForNodesReady(ctx context.Context, input WaitForNodesReadyInput) {
	Eventually(func() (bool, error) {
		nodeList := &corev1.NodeList{}
		if err := input.Lister.List(ctx, nodeList); err != nil {
			return false, err
		}
		nodeReadyCount := 0
		for _, node := range nodeList.Items {
			fmt.Fprintf(GinkgoWriter, "KubeletVersions: %s, KubernetesVersion: %s\n", semver.MajorMinor(node.Status.NodeInfo.KubeletVersion), semver.MajorMinor(input.KubernetesVersion))
			if !(semver.MajorMinor(node.Status.NodeInfo.KubeletVersion) == semver.MajorMinor(input.KubernetesVersion)) {
				return false, nil
			}
			fmt.Fprintf(GinkgoWriter, "node %s is ready: %t\n", node.Name, noderefutil.IsNodeReady(&node))
			if !noderefutil.IsNodeReady(&node) {
				return false, nil
			}
			nodeReadyCount++
		}
		fmt.Fprintf(GinkgoWriter, "nodeReadyCount: %d, expected count: %d\n", nodeReadyCount, input.Count)
		return input.Count == nodeReadyCount, nil
	}, input.WaitForNodesReady...).Should(BeTrue())
}

// byClusterOptions returns a set of ListOptions that allows to identify all the objects belonging to a Cluster.
func byClusterOptions(name, namespace string) []client.ListOption {
	return []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: name,
		},
	}
}

func isMachineInList(machine clusterv1.Machine, list *clusterv1.MachineList) bool {
	if list == nil {
		return false
	}

	for _, m := range list.Items {
		if m.Name == machine.Name {
			return true
		}
	}
	return false
}
