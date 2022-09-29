/*
Copyright 2022 The Koordinator Authors.

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

package batchresource

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	resschedplug "k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"

	apiext "github.com/koordinator-sh/koordinator/apis/extension"
)

const (
	Name = "BatchResourceFit"
)

type batchResource struct {
	MilliCPU int64
	Memory   int64
}

var (
	_ framework.FilterPlugin = &Plugin{}
)

type Plugin struct {
}

func New(args runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	return &Plugin{}, nil
}

func (p *Plugin) Name() string {
	return Name
}

func (p *Plugin) Filter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	insufficientResources := fitsRequest(pod, nodeInfo)

	// 如果发现当前 pod 资源申请超过 node 剩余资源，则剔除当前 node
	if len(insufficientResources) != 0 {
		// We will keep all failure reasons.
		failureReasons := make([]string, 0, len(insufficientResources))
		for _, r := range insufficientResources {
			failureReasons = append(failureReasons, r.Reason)
		}
		return framework.NewStatus(framework.Unschedulable, failureReasons...)
	}
	return nil
}

// fitsRequest 用于校验 batch 类型 pod 资源
func fitsRequest(pod *corev1.Pod, nodeInfo *framework.NodeInfo) []resschedplug.InsufficientResource {
	// 计算当前 pod batch 类型资源
	podBatchRequest := computePodBatchRequest(pod)
	// 如果 pod resource 配置不包含 batch 配置，则直接跳过
	if podBatchRequest.MilliCPU == 0 && podBatchRequest.Memory == 0 {
		return nil
	}

	insufficientResources := make([]resschedplug.InsufficientResource, 0, 2)
	// 从 node 状态中获取当前 batch 类型 pod 已申请资源
	nodeRequested := computeNodeBatchRequested(nodeInfo)
	// 从 node 状态中获取预留 batch 类型 pod 资源总数
	nodeAllocatable := computeNodeBatchAllocatable(nodeInfo)

	// 如果当前 pod 申请资源超过当前节点为 batch 类型 pod 预留资源，则判定为资源溢出
	if podBatchRequest.MilliCPU > (nodeAllocatable.MilliCPU - nodeRequested.MilliCPU) {
		insufficientResources = append(insufficientResources, resschedplug.InsufficientResource{
			ResourceName: apiext.BatchCPU,
			Reason:       "Insufficient batch cpu",
			Requested:    podBatchRequest.MilliCPU,
			Used:         nodeRequested.MilliCPU,
			Capacity:     nodeAllocatable.MilliCPU,
		})
	}
	if podBatchRequest.Memory > (nodeAllocatable.Memory - nodeRequested.Memory) {
		insufficientResources = append(insufficientResources, resschedplug.InsufficientResource{
			ResourceName: apiext.BatchMemory,
			Reason:       "Insufficient batch memory",
			Requested:    podBatchRequest.Memory,
			Used:         nodeRequested.Memory,
			Capacity:     nodeAllocatable.Memory,
		})
	}
	return insufficientResources
}

func computeNodeBatchAllocatable(nodeInfo *framework.NodeInfo) *batchResource {
	nodeAllocatable := &batchResource{
		MilliCPU: 0,
		Memory:   0,
	}
	// compatible with old format, overwrite BatchCPU, BatchMemory if exist
	// nolint:staticcheck // SA1019: apiext.KoordBatchCPU is deprecated: because of the limitation of extended resource naming
	if koordBatchCPU, exist := nodeInfo.Allocatable.ScalarResources[apiext.KoordBatchCPU]; exist {
		nodeAllocatable.MilliCPU = koordBatchCPU
	}
	// nolint:staticcheck // SA1019: apiext.KoordBatchMemory is deprecated: because of the limitation of extended resource naming
	if koordBatchMemory, exist := nodeInfo.Allocatable.ScalarResources[apiext.KoordBatchMemory]; exist {
		nodeAllocatable.Memory = koordBatchMemory
	}
	if batchCPU, exist := nodeInfo.Allocatable.ScalarResources[apiext.BatchCPU]; exist {
		nodeAllocatable.MilliCPU = batchCPU
	}
	if batchMemory, exist := nodeInfo.Allocatable.ScalarResources[apiext.BatchMemory]; exist {
		nodeAllocatable.Memory = batchMemory
	}
	return nodeAllocatable
}

func computeNodeBatchRequested(nodeInfo *framework.NodeInfo) *batchResource {
	nodeRequested := &batchResource{
		MilliCPU: 0,
		Memory:   0,
	}
	// compatible with old format, accumulate
	// with KoordBatchCPU, KoordBatchCPU if exist
	if batchCPU, exist := nodeInfo.Requested.ScalarResources[apiext.BatchCPU]; exist {
		nodeRequested.MilliCPU += batchCPU
	}
	// nolint:staticcheck // SA1019: apiext.KoordBatchCPU is deprecated: because of the limitation of extended resource naming
	if koordBatchCPU, exist := nodeInfo.Requested.ScalarResources[apiext.KoordBatchCPU]; exist {
		nodeRequested.MilliCPU += koordBatchCPU
	}
	if batchMemory, exist := nodeInfo.Requested.ScalarResources[apiext.BatchMemory]; exist {
		nodeRequested.Memory += batchMemory
	}
	// nolint:staticcheck // SA1019: apiext.KoordBatchMemory is deprecated: because of the limitation of extended resource naming
	if koordBatchMemory, exist := nodeInfo.Requested.ScalarResources[apiext.KoordBatchMemory]; exist {
		nodeRequested.Memory += koordBatchMemory
	}
	return nodeRequested
}

// computePodBatchRequest returns the total non-zero best-effort requests. If Overhead is defined for the pod and
// the PodOverhead feature is enabled, the Overhead is added to the result.
// podBERequest = max(sum(podSpec.Containers), podSpec.InitContainers) + overHead
func computePodBatchRequest(pod *corev1.Pod) *batchResource {
	podRequest := &framework.Resource{}
	// 统计所有容器资源配置，包括 cpu/mem
	for _, container := range pod.Spec.Containers {
		podRequest.Add(container.Resources.Requests)
	}

	// take max_resource(sum_pod, any_init_container)
	// 获取初始化容器资源配置，如果大于上一步计算结果，则覆盖现在资源配置
	for _, container := range pod.Spec.InitContainers {
		podRequest.SetMaxResource(container.Resources.Requests)
	}

	// If Overhead is being utilized, add to the total requests for the pod
	// 如果 Overhead 特性开启，则需要将 Overhead 资源追加到 pod 资源
	if pod.Spec.Overhead != nil {
		podRequest.Add(pod.Spec.Overhead)
	}

	result := &batchResource{
		MilliCPU: 0,
		Memory:   0,
	}
	// compatible with old format, overwrite BatchCPU, BatchMemory if exist
	// nolint:staticcheck // SA1019: apiext.KoordBatchCPU is deprecated: because of the limitation of extended resource naming
	// 从 pod 资源配置中获取自定义资源，此处存在两个版本自定义资源名称，所以此处需要每种资源获取两次
	if koordBatchCPU, exist := podRequest.ScalarResources[apiext.KoordBatchCPU]; exist {
		result.MilliCPU = koordBatchCPU
	}
	// nolint:staticcheck // SA1019: apiext.KoordBatchMemory is deprecated: because of the limitation of extended resource naming
	if koordBatchMemory, exist := podRequest.ScalarResources[apiext.KoordBatchMemory]; exist {
		result.Memory = koordBatchMemory
	}

	if batchCPU, exist := podRequest.ScalarResources[apiext.BatchCPU]; exist {
		result.MilliCPU = batchCPU
	}
	if batchMemory, exist := podRequest.ScalarResources[apiext.BatchMemory]; exist {
		result.Memory = batchMemory
	}
	return result
}
