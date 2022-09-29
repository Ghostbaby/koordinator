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

package loadaware

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	resourceapi "k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/koordinator-sh/koordinator/apis/extension"
	"github.com/koordinator-sh/koordinator/apis/scheduling/config"
	"github.com/koordinator-sh/koordinator/apis/scheduling/config/validation"
	slov1alpha1 "github.com/koordinator-sh/koordinator/apis/slo/v1alpha1"
	slolisters "github.com/koordinator-sh/koordinator/pkg/client/listers/slo/v1alpha1"
	"github.com/koordinator-sh/koordinator/pkg/scheduler/frameworkext"
)

const (
	Name                          = "LoadAwareScheduling"
	ErrReasonNodeMetricExpired    = "node(s) nodeMetric expired"
	ErrReasonUsageExceedThreshold = "node(s) %s usage exceed threshold"
)

const (
	// DefaultMilliCPURequest defines default milli cpu request number.
	DefaultMilliCPURequest int64 = 250 // 0.25 core
	// DefaultMemoryRequest defines default memory request size.
	DefaultMemoryRequest int64 = 200 * 1024 * 1024 // 200 MB
	// DefaultNodeMetricReportInterval defines the default koodlet report NodeMetric interval.
	DefaultNodeMetricReportInterval = 60 * time.Second
)

var (
	_ framework.FilterPlugin  = &Plugin{}
	_ framework.ScorePlugin   = &Plugin{}
	_ framework.ReservePlugin = &Plugin{}
)

type Plugin struct {
	handle           framework.Handle
	args             *config.LoadAwareSchedulingArgs
	nodeMetricLister slolisters.NodeMetricLister
	podAssignCache   *podAssignCache
}

func New(args runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	pluginArgs, ok := args.(*config.LoadAwareSchedulingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type LoadAwareSchedulingArgs, got %T", args)
	}

	if err := validation.ValidateLoadAwareSchedulingArgs(pluginArgs); err != nil {
		return nil, err
	}

	frameworkExtender, ok := handle.(frameworkext.ExtendedHandle)
	if !ok {
		return nil, fmt.Errorf("want handle to be of type frameworkext.ExtendedHandle, got %T", handle)
	}

	assignCache := newPodAssignCache()
	frameworkExtender.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(assignCache)
	nodeMetricLister := frameworkExtender.KoordinatorSharedInformerFactory().Slo().V1alpha1().NodeMetrics().Lister()

	return &Plugin{
		handle:           handle,
		args:             pluginArgs,
		nodeMetricLister: nodeMetricLister,
		podAssignCache:   assignCache,
	}, nil
}

func (p *Plugin) Name() string { return Name }

func (p *Plugin) Filter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	// 获取本次过滤 node 信息
	node := nodeInfo.Node()
	if node == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}

	// 获取 node 对应自定义 nodeMetric 对象，该对象主要用于描述节点资源使用详情，是原生 node 资源的拓展
	nodeMetric, err := p.nodeMetricLister.Get(node.Name)
	if err != nil {
		// For nodes that lack load information, fall back to the situation where there is no load-aware scheduling.
		// Some nodes in the cluster do not install the koordlet, but users newly created Pod use koord-scheduler to schedule,
		// and the load-aware scheduling itself is an optimization, so we should skip these nodes.
		if errors.IsNotFound(err) {
			return nil
		}
		return framework.NewStatus(framework.Error, err.Error())
	}

	// 如果当前调度插件对 node metrics 有时效性要求，则根据 KubeSchedulerConfiguration 配置中参数进行校验
	// 主要校验当前 node metrics 是否过期
	if p.args.FilterExpiredNodeMetrics != nil && *p.args.FilterExpiredNodeMetrics && p.args.NodeMetricExpirationSeconds != nil {
		if isNodeMetricExpired(nodeMetric, *p.args.NodeMetricExpirationSeconds) {
			return framework.NewStatus(framework.Unschedulable, ErrReasonNodeMetricExpired)
		}
	}

	// 获取当前调度插件对节点负载设置各种资源的上限
	usageThresholds := p.args.UsageThresholds
	// 如果原生 node 资源存在 AnnotationCustomUsageThresholds 标签，则使用 node annotation 覆盖 KubeSchedulerConfiguration 中定义参数
	customUsageThresholds, err := extension.GetCustomUsageThresholds(node)
	if err != nil {
		klog.V(5).ErrorS(err, "failed to GetCustomUsageThresholds from", "node", node.Name)
	} else {
		if len(customUsageThresholds.UsageThresholds) > 0 {
			usageThresholds = customUsageThresholds.UsageThresholds
		}
	}

	// 如果当前节点资源限制存在，则进行资源配额校验
	if len(usageThresholds) > 0 {
		if nodeMetric.Status.NodeMetric == nil {
			return nil
		}
		for resourceName, threshold := range usageThresholds {
			if threshold == 0 {
				continue
			}
			total := node.Status.Allocatable[resourceName]
			if total.IsZero() {
				continue
			}

			// 校验当前节点资源使用率是否超过限额，如果超过跳过当前节点
			used := nodeMetric.Status.NodeMetric.NodeUsage.ResourceList[resourceName]
			usage := int64(math.Round(float64(used.MilliValue()) / float64(total.MilliValue()) * 100))
			if usage >= threshold {
				return framework.NewStatus(framework.Unschedulable, fmt.Sprintf(ErrReasonUsageExceedThreshold, resourceName))
			}
		}
	}

	return nil
}

func (p *Plugin) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (p *Plugin) Reserve(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string) *framework.Status {
	p.podAssignCache.assign(nodeName, pod)
	return nil
}

func (p *Plugin) Unreserve(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string) {
	p.podAssignCache.unAssign(nodeName, pod)
}

func (p *Plugin) Score(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string) (int64, *framework.Status) {
	// 根据 node 名称获取原生 node 资源，即获取原生 node 资源使用情况
	nodeInfo, err := p.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}
	node := nodeInfo.Node()
	if node == nil {
		return 0, framework.NewStatus(framework.Error, "node not found")
	}

	// 获取当前 node metrics
	nodeMetric, err := p.nodeMetricLister.Get(nodeName)
	if err != nil {
		// caused by load-aware scheduling itself is an optimization,
		// so we should skip the node and score the node 0
		if errors.IsNotFound(err) {
			return 0, nil
		}
		return 0, framework.NewStatus(framework.Error, err.Error())
	}

	// 校验当前 node metrics 是否过期
	if p.args.NodeMetricExpirationSeconds != nil && isNodeMetricExpired(nodeMetric, *p.args.NodeMetricExpirationSeconds) {
		return 0, nil
	}

	// 获取当前 pod 预计使用资源信息，按照配置权重对申请资源进行折算
	estimatedUsed := estimatedPodUsed(pod, p.args.ResourceWeights, p.args.EstimatedScalingFactors)

	// 计算 nodeMetric 更新窗口中被分配的 pod 资源使用量
	estimatedAssignedPodUsage := p.estimatedAssignedPodUsage(nodeName, nodeMetric)

	// 当前 pod 资源使用量 + nodeMetrics 不包含已经分配到当前节点 pod 资源使用量
	for resourceName, value := range estimatedAssignedPodUsage {
		estimatedUsed[resourceName] += value
	}

	// allocatable 为当前 node 预留用于调度 pod 的资源总和
	allocatable := make(map[corev1.ResourceName]int64)
	for resourceName := range p.args.ResourceWeights {
		quantity := node.Status.Allocatable[resourceName]
		if resourceName == corev1.ResourceCPU {
			allocatable[resourceName] = quantity.MilliValue()
		} else {
			allocatable[resourceName] = quantity.Value()
		}
		// 本次调度预期分配资源 + 操作系统资源使用量
		if nodeMetric.Status.NodeMetric != nil {
			quantity = nodeMetric.Status.NodeMetric.NodeUsage.ResourceList[resourceName]
			if resourceName == corev1.ResourceCPU {
				estimatedUsed[resourceName] += quantity.MilliValue()
			} else {
				estimatedUsed[resourceName] += quantity.Value()
			}
		}
	}

	// 计算当前 node 调度分数
	score := loadAwareSchedulingScorer(p.args.ResourceWeights, estimatedUsed, allocatable)
	return score, nil
}

func isNodeMetricExpired(nodeMetric *slov1alpha1.NodeMetric, nodeMetricExpirationSeconds int64) bool {
	return nodeMetric == nil ||
		nodeMetric.Status.UpdateTime == nil ||
		nodeMetricExpirationSeconds > 0 &&
			time.Since(nodeMetric.Status.UpdateTime.Time) >= time.Duration(nodeMetricExpirationSeconds)*time.Second
}

// estimatedAssignedPodUsage 计算 nodeMetric 更新窗口中被分配的 pod 资源使用量
func (p *Plugin) estimatedAssignedPodUsage(nodeName string, nodeMetric *slov1alpha1.NodeMetric) map[corev1.ResourceName]int64 {
	estimatedUsed := make(map[corev1.ResourceName]int64)
	nodeMetricReportInterval := getNodeMetricReportInterval(nodeMetric)
	p.podAssignCache.lock.RLock()
	defer p.podAssignCache.lock.RUnlock()
	for _, assignInfo := range p.podAssignCache.podInfoItems[nodeName] {
		// 下面两种情况需要追加上 podAssignCache 中 pod：
		// 1. 如果当前已经调度 pod 时间大于 nodeMetrics 更新时间
		// 2. pod 调度时间 小于 nodeMetrics 更新时间，但是两个时间点相差时间 小于 nodeMetrics 上报间隔
		if assignInfo.timestamp.After(nodeMetric.Status.UpdateTime.Time) ||
			assignInfo.timestamp.Before(nodeMetric.Status.UpdateTime.Time) &&
				nodeMetric.Status.UpdateTime.Sub(assignInfo.timestamp) < nodeMetricReportInterval {
			estimated := estimatedPodUsed(assignInfo.pod, p.args.ResourceWeights, p.args.EstimatedScalingFactors)
			for resourceName, value := range estimated {
				estimatedUsed[resourceName] += value
			}
		}
	}
	return estimatedUsed
}

func getNodeMetricReportInterval(nodeMetric *slov1alpha1.NodeMetric) time.Duration {
	if nodeMetric.Spec.CollectPolicy == nil || nodeMetric.Spec.CollectPolicy.ReportIntervalSeconds == nil {
		return DefaultNodeMetricReportInterval
	}
	return time.Duration(*nodeMetric.Spec.CollectPolicy.ReportIntervalSeconds) * time.Second
}

// estimatedPodUsed 计算单个 pod 预计资源使用量
func estimatedPodUsed(pod *corev1.Pod, resourceWeights map[corev1.ResourceName]int64, scalingFactors map[corev1.ResourceName]int64) map[corev1.ResourceName]int64 {
	// 获取 pod 资源 request 、 limit
	requests, limits := resourceapi.PodRequestsAndLimits(pod)
	estimatedUsed := make(map[corev1.ResourceName]int64)
	// 获取 pod priority claas
	priorityClass := extension.GetPriorityClass(pod)
	for resourceName := range resourceWeights {
		// 根据 pod priority claas 获取对应 pod ResourceName
		// pod resourceList 类型为 map[ResourceName]resource.Quantity，此处的 ResourceName 可以自定义
		realResourceName := extension.TranslateResourceNameByPriorityClass(priorityClass, resourceName)
		// 根据上一步获取的资源名称，获取对应资源的预计使用量
		estimatedUsed[resourceName] = estimatedUsedByResource(requests, limits, realResourceName, scalingFactors[resourceName])
	}
	return estimatedUsed
}

// estimatedUsedByResource 预估资源使用量
func estimatedUsedByResource(requests, limits corev1.ResourceList, resourceName corev1.ResourceName, scalingFactor int64) int64 {
	limitQuantity := limits[resourceName]
	requestQuantity := requests[resourceName]
	var quantity resource.Quantity
	if limitQuantity.Cmp(requestQuantity) > 0 {
		scalingFactor = 100
		quantity = limitQuantity
	} else {
		quantity = requestQuantity
	}

	// 如果 pod 没有配置资源限制，则按照默认值计算
	if quantity.IsZero() {
		switch resourceName {
		case corev1.ResourceCPU, extension.BatchCPU:
			return DefaultMilliCPURequest
		case corev1.ResourceMemory, extension.BatchMemory:
			return DefaultMemoryRequest
		}
		return 0
	}

	var estimatedUsed int64
	switch resourceName {
	case corev1.ResourceCPU:
		estimatedUsed = int64(math.Round(float64(quantity.MilliValue()) * float64(scalingFactor) / 100))
		if estimatedUsed > limitQuantity.MilliValue() {
			estimatedUsed = limitQuantity.MilliValue()
		}
	default:
		estimatedUsed = int64(math.Round(float64(quantity.Value()) * float64(scalingFactor) / 100))
		if estimatedUsed > limitQuantity.Value() {
			estimatedUsed = limitQuantity.Value()
		}
	}
	return estimatedUsed
}

// loadAwareSchedulingScorer 计算当前节点得分
// 计算逻辑：（cpu 得分 + mem 得分） / （cpu 权重 + mem 权重）
func loadAwareSchedulingScorer(resToWeightMap map[corev1.ResourceName]int64, used, allocatable map[corev1.ResourceName]int64) int64 {
	var nodeScore, weightSum int64
	for resourceName, weight := range resToWeightMap {
		// 计算单中资源节点得分
		resourceScore := leastRequestedScore(used[resourceName], allocatable[resourceName])
		// 按照配置权重进行分数折算
		nodeScore += resourceScore * weight
		// 累计权重总和
		weightSum += weight
	}
	// 汇总所有资源得分，取平均值
	return nodeScore / weightSum
}

// leastRequestedScore 计算单独资源分数
// 计算公式：（（节点总资源 - 已经分配）* 节点满分）/ 节点总资源
func leastRequestedScore(requested, capacity int64) int64 {
	if capacity == 0 {
		return 0
	}
	if requested > capacity {
		return 0
	}

	return ((capacity - requested) * framework.MaxNodeScore) / capacity
}
