---
title: Gang scheduling
authors:
  - "@buptcozy"
reviewers:
  - "@eahydra"
  - "@hormes"
  - "@yihuifeng"
  - "@honpey"
  - "@zwzhang0107"
  - "@jasonliu747"
creation-date: 2022-07-01
last-updated: 2022-07-01

---

# Gang scheduling

## Table of Contents

<!--ts-->

* [Gang scheduling](#Gang-scheduling)
    * [Table of Contents](#table-of-contents)
    * [Summary](#概览)
    * [Motivation](#起因)
      * [Compared with competitors](#竞品对标)
        * [Coscheduling](#Coscheduling)
      * [Goals](#目标)
      * [Non Goals and Future Work](#Non-Goals-and-Future-Work)
    * [Proposal](#方案)
      * [Key concept](#核心概念)
        * [Strict and NonStrict](#Strict/NonStrict)
        * [GangGroup](#ganggroup)
        * [After gang](#after-gang)
      * [API](#API)
        * [Definition](#definition)
          * [CRD way](#crd-way)
            * [Example](#example)  
          * [Annotation way](#annotation-way)
            * [Example](#example)         
      * [Implementation Details](#详细设计)
        * [QueueSortPlugin](#QueueSortPlugin)
        * [Data-Structure](#data-structure)
        * [GangPlugin](#gang-plugin)
    * [Unsolved Problems](#Unsolved-Problems)
    * [Alternatives](#Alternatives)
    * [Implementation History](#Implementation-History)
    * [References](#References)
<!--te-->

## 概览
本方案为 pod 节点调度绑定提供 Gang 机制。用户可以声明最小资源集合数（resource-collection-minimum），只有当已经完成调度资源数（assigned-resources）超过前面声明当前最小资源集合数才能触发节点绑定。
同时提供 `Strict` 和 `NonStrict` 两个参数用于控制 resource-accumulation-process ，区别于其他社区方案将提供 two-level Gang 描述用于更好匹配真实场景。

## 起因
在 AI 场景中很多任务都需要使用 Gang scheduling，社区已经有很多相关实现，比如 `Coscheduling` 、 `vocalno`，设计过程中我们从社区项目中得到了很多灵感。

### 竞品对标

#### Coscheduling

1. `Coscheduling` 主要通过实现新型队列排序（queue-sort）接口以及其他方法将一组 Gang pod 尽量有序得出队。
举个🌰 ，我们有 10 个任务需要进行 Gang 调度，前面 5 个任务已经调度成功，此时第 6 个任务调度失败，`Coscheduling` 将会回滚前面 5 个已经完成调度的任务，同时会跳过后面 4 个调度中的任务。
2. `Coscheduling` 会简单得使用一个全局间隔时间作为 Gang 调度周期。该设计会带来两个问题：
   - 问题一，如果配置间隔太长会带来无效等待，如果太短会带来无效调度。
   - 问题二，如果待调度分组任务很多，此时大概率会出现周期内无法完成调度，出现调度超时的情况。
   对于上面的场景，我们的设计中称为 `Strict`，此场景下调度会严格按照既定配置的周期时间进行工作。
3. 有些任务需要复杂的 Gang 要求。例如，一个任务有几个规则，每个规则都有几个 pod 以及自身的 Gang 条件，任务也需要不同的规则来组成不同的 GangGroups。
一个 GangGroup 中的所有 pod 只有在 GangGroup 中的所有规则都满足 Gang 条件后才触发绑定过程。`Coscheduling` 不能满足这个需求。

### 目标
1. 定义 Gang 调度配置。

2. 提供调度器插件实现 Gang 调度。

### Non Goals and Future Work
1. Provide ability to solve Gang resource deadlock problems with `NonStrict`.

## 方案

### 核心概念

#### Strict/NonStrict

`Strict` 模式，如果其中一个 pod 调度失败，当前调度周期内，其他已经调度成功的 pod 将会被取消调度，同时正在调度中的 pod 将会在 PreFilter 阶段被拒绝调度。

`NonStrict` 模式，如果其中一个 pod 调度失败，并不会影响其他 pod 参与调度，会继续累计已经被调度的 pod 直到符合 Gang 调度条件。 
    此模式对于 pod 比较多的情况比较友好，但是会增加不同 Gang 调度之间资源死锁的风险。
> 举个🌰 ，如果当前资源配额为 10，此时用户提交三组 Gang 调度任务 pod 数都为 5，由于各种条件限制，Gang 调度 1/2/3 任务分别调度起来 pod 数量为 3/3/4，
> 此时当前资源组配额已经耗尽，不会有新到 pod 完成调度，三组 Gang 调度任务就会一直出于等待状态，这就是上面说到到资源死锁情况，目前还没有这个问题。

#### GangGroup

`GangGroup`，有些任务需要复杂的 Gang 要求。例如，一个任务有几个规则，每个规则都有几个 pod 以及自身的 Gang 条件，任务也需要不同的规则来组成不同的 GangGroups。
一个 GangGroup 中的所有 pod 只有在 GangGroup 中的所有规则都满足 Gang 条件后才触发绑定过程。`GangGroup` 则允许我们将不同 Gangs 进行聚合。

#### After Gang

注意⚠️，如果满足 Gang 调度资源积累条件，随后一些 pod 在 binding 阶段失败，或者一些已经绑定的 pod 被抢占或者重新调度，这种情况下 Gang 的约束在资源重新分配过程中是否依然有效？
答案：应该不时效，因为 Gang 的设计初衷要求所有 pod 需要同时被拉起，如果只有其中一些 pod 被拉起，那么后续操作继续执行 Gang 调度策略将失去意义。
     因此，一旦 Gang 策略已经满足，后续所有的资源分配将不受 Gang 规则约束，后续将使用默认调度进行 pod 调度。

#### WaitTime

`WaitTime` 自第一个 pod 进入 permit 阶段依赖的最大等待时间。如果 `WaitTime` 已经超时，调度器将会回滚所有已经调度完成的 pod，
    并且更新所有 pod annotation `gang.scheduling.koordinator.sh/timeout=true`，调度器将不会再调度这些 pod。用户需要注意这种情况并及时删除此类 pod。

### API
#### 定义

我们设计的初衷是优化以及增强社区原有的 `PodGroup` 能力，所以我们的 `PodGroup` 定义会兼容社区设计。我们会提供通过使用更新 annotation 方式使用 Gang 调度特性。

#### CRD 方式

用户可以使用社区 `PodGroup` CRD 声明 Gang：
```go

type PodGroup struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec PodGroupSpec `json:"spec,omitempty"`
    Status PodGroupStatus `json:"status,omitempty"`
}

type PodGroupSpec struct {
    MinMember int32 `json:"minMember,omitempty"`
    MinResources *v1.ResourceList `json:"minResources,omitempty"`
    
    ScheduleTimeoutSeconds *int32 `json:"scheduleTimeoutSeconds,omitempty"`
}

```

Pod 需要添加 label `pod-group.scheduling.sigs.k8s.io` 来关联 `PodGroup` 配置。

同时，我们也可以使用以下可选配置：
```yaml
gang.scheduling.koordinator.sh/total-number
gang.scheduling.koordinator.sh/mode        
gang.scheduling.koordinator.sh/groups
```

- `gang.scheduling.koordinator.sh/name` 配置 Gang 调度器名称, 名称需要符合 RFC 1123 规范。

- `gang.scheduling.koordinator.sh/total-number` 当前配置仅作用于 `Strict` 模式， 详情请参考 `Data-Structure` 部分。默认与 `gang.scheduling.koordinator.sh/min-available` 一致。

- `gang.scheduling.koordinator.sh/mode` 选项 `Strict` 或者 `NonStrict`。 默认配置为 `Strict`。

- `gang.scheduling.koordinator.sh/groups` 用于配置 GangGroups 名称。默认为空，表示不需要与其他资源合并到 GangGroups，同一个 GangGroups 的 Gangs 可以来自于不同的 namespace。

`PodGroup` annotation 可以包含 `gang.scheduling.koordinator.sh/total-number`， `gang.scheduling.koordinator.sh/mode`， `gang.scheduling.koordinator.sh/gang-groups`


##### Example
以下为基础 Gang 调度配置:
```yaml
apiVersion: v1alpha1
kind: PodGroup
metadata:
  name: gang-a
  namespace: default
spec:
  minMember: 5
  minResources:
    cpu: "5"
    memory: "2048Mi"
  scheduleTimeoutSeconds: 600
```

创建一个任务包含两个策略：A 和 B，每个策略包含一些 pod。PodA 属于 roleA，PodB 属于 roleB。roleA、roleB 归属于同一个 GangGroup，示例如下：

```yaml
apiVersion: v1alpha1
kind: PodGroup
metadata:
  name: gang-a
  namespace: namespaceA
  annotations:
    gang.scheduling.koordinator.sh/total-number: 5
    gang.scheduling.koordinator.sh/mode: Strict
    gang.scheduling.koordinator.sh/groups: ["namespaceA/gang-a", "namespaceB/gang-b"]
spec:
  minMember: 5
  minResources:
    cpu: "5"
    memory: "2048Mi"
  scheduleTimeoutSeconds: 600
```

如果用户使用 `CRD way`，需要提前将 PodGroup 策略部署到集群，否则会出现带有 Gang 配置的 pod 进行调度时，找不到对应的 Gang 策略 PodGroup 配置。

#### Annotation 方式
```yaml
gang.scheduling.koordinator.sh/name           
gang.scheduling.koordinator.sh/min-available
```

以上配置为必填，同时我们兼容社区 annotation `pod-group.scheduling.sigs.k8s.io`， `pod-group.scheduling.sigs.k8s.io/name`以及 `pod-group.scheduling.sigs.k8s.io/min-available` 。

同时，我们还支持以下可选配置：
```yaml
gang.scheduling.koordinator.sh/waiting-time
gang.scheduling.koordinator.sh/total-number
gang.scheduling.koordinator.sh/mode        
gang.scheduling.koordinator.sh/groups
```

- `gang.scheduling.koordinator.sh/waiting-time` 自第一个 pod 进入 permit 阶段依赖的最大等待时间。默认值可以在全局配置中设置。

- `gang.scheduling.koordinator.sh/total-number` 当前配置仅作用于 `Strict` 模式， 详情请参考 `Data-Structure` 部分。默认与 `gang.scheduling.koordinator.sh/min-available` 一致。

- `gang.scheduling.koordinator.sh/mode` 选项 `Strict` 或者 `NonStrict`。 默认配置为 `Strict`。

- `gang.scheduling.koordinator.sh/groups` 用于配置 GangGroups 名称。默认为空，表示不需要与其他资源合并到 GangGroups，同一个 GangGroups 的 Gangs 可以来自于不同的 namespace。

注意⚠️，如果同时通过 CRD 和 annotation 方式进行配置，该 annotation 配置将会覆盖 CRD 配置。同时， GangGroup 名称格式为 " gangNamespace" + "/" + "gangName "

##### Example
以下为基础 Gang 调度配置:
```yaml
metadata:
   annotations:
    gang.scheduling.koordinator.sh/name: gang-a
    gang.scheduling.koordinator.sh/min-available: 5
```

创建一个任务包含两个策略：A 和 B，每个策略包含一些 pod。PodA 属于 roleA，PodB 属于 roleB。roleA、roleB 归属于同一个 GangGroup，示例如下：
```yaml
metadata:
   annotations:
     gang.scheduling.koordinator.sh/name: gang-a
     gang.scheduling.koordinator.sh/waiting-time: 3600s 
     gang.scheduling.koordinator.sh/min-available: 5
     gang.scheduling.koordinator.sh/total-number: 5
     gang.scheduling.koordinator.sh/mode: Strict
     gang.scheduling.koordinator.sh/groups: ["namespaceA/gang-a", "namespaceB/gang-b"]
metadata:
   annotations:
     gang.scheduling.koordinator.sh/name: gang-b
     gang.scheduling.koordinator.sh/waiting-time: 3600s 
     gang.scheduling.koordinator.sh/min-available: 5
     gang.scheduling.koordinator.sh/total-number: 5
     gang.scheduling.koordinator.sh/mode: Strict
     gang.scheduling.koordinator.sh/groups: ["namespaceA/gang-a", "namespaceB/gang-b"]
```

创建一个任务包含两个策略：A 和 B，每个策略包含一些 pod。PodA 属于 roleA，PodB 属于 roleB。roleA、roleB 归属于不同 GangGroup，示例如下：
```yaml
metadata:
  annotations:
     gang.scheduling.koordinator.sh/name: gang-a
     gang.scheduling.koordinator.sh/waiting-time: 3600s 
     gang.scheduling.koordinator.sh/min-available: 5
     gang.scheduling.koordinator.sh/total-number: 5
     gang.scheduling.koordinator.sh/mode: Strict
     gang.scheduling.koordinator.sh/groups: ""
metadata:
   annotations:
     gang.scheduling.koordinator.sh/name: gang-b
     gang.scheduling.koordinator.sh/waiting-time: 3600s 
     gang.scheduling.koordinator.sh/min-available: 5
     gang.scheduling.koordinator.sh/total-number: 5
     gang.scheduling.koordinator.sh/mode: Strict
     gang.scheduling.koordinator.sh/groups: ""
```

### 详细设计
#### QueueSortPlugin

我们单独设计调度器插件用于实现 `QueueSort` 拓展点，这样就可以将队列排序逻辑集成到所有插件，并且只需要注册一次。

当前方案中，我们实现 Less 方法汇总属于相同 Gang 的 pod。具体排序规则为：

1. 比较两个 pod 的优先级配置，优先级越高的 pod 优先入队。
2. 比较两个 pod 的创建时间戳，如果 pod 归属于同一个 Gang 配置，我们比较 Gang 配置创建时间，谁先创建则优先入队。
3. 比较 pod 的 namespace，如果 pod 归属某一个 Gang 配置，则比较 Gang 名称。

```go
type QueueSortPlugin interface{
    QueueSort(*QueuedPodInfo, *QueuedPodInfo) bool
}
```

#### GangSchedulingPlugin
##### Data-Structure
###### Gang
```go
type Gang struct {
    Name                         string                
    WaitTime                     time.Duration                       
    Mode                         string                 //Strict or NonStrict
    GangGroup                    []string               
    MinRequiredNumber            int                    
    TotalChildrenNum             int
    Children                     map[string]*PodInfo  
    BoundChildren                map[string]*PodInfo
    WaitingForBindChildren       map[string]*PodInfo
    ResourceSatisfied            bool 
    ScheduleCycle                int
    ScheduleCycleValid           bool
    ChildrenScheduleRoundMap     map[string]int
}
```

Gang，用于记录 Gang 调度状态到调度器缓存。

- `Children`，用于记录归属于当前 Gang 的 pod 列表
- `BoundChildren`，`WaitingForBindChildren` 用于记录已经出于 binding 状态的 pod，用于检查 pod 是否已经通过 permit 阶段。
- `ResourceSatisfied`，用于标记当前 pod 是否通过调度 Permit 阶段，如果通过则为 true。该字段主要用于判断当前 Gang 调度是否满足条件。
- `scheduleCycle`，`childrenScheduleRoundMap`，前面两个字段主要用于控制 Gang 调度周期。
> 举个🌰 ，调度伊始 `scheduleCycle` 字段为 1，`childrenScheduleRoundMap` 中所有 pod 值为 0。
> 所有 pod 进入 PreFilter 阶段时，将会判断 `childrenScheduleRoundMap` 中 pod 值是否小于 `scheduleCycle` 值；
> 如果上一步校验通过，则将 `childrenScheduleRoundMap` 值设置为 `scheduleCycle` 的值，并通过当前校验；
> 反之则说明当前 pod 在本轮调度周期内已经完成调度，需要拒绝本次调度。
> 根据 `totalChildrenNum` 字段，当所有 pod 都通过 PreFilter 阶段，说明当前调度周期所有 pod 已经完成调度，`scheduleCycle` 需要累加 1，说明开启新一轮调度周期。
- `scheduleCycleValid`，当前 Gang 中任意 pod 在 Filter 阶段失败，scheduleCycleValid 将设置为 true，只有所有 pod 全部通过 Filter 阶段，该字段才会设置为 true。
  `scheduleCycleValid=false` 此场景下所有 pod 将不会进行调度，同时所有调度中都 pod 将被在 PreFilter 阶段被拒绝，当新一轮调度周期开启时，`scheduleCycleValid` 才会被设置为 true。

注意⚠️ ，`scheduleCycle\scheduleCycleValid\childrenScheduleRoundMap` 仅作用于 `Strict` 模式。

##### GangPlugin

在调度器框架 Plugin 结构提基础上，增加 gangCache 用于缓存 Gang 信息。
```go
type GangPlugin struct {
    frameworkHandler            framework.Handle
    gangClient                  gangClient.Interface
    podLister                   listerv1.PodLister
    snapshotSharedLister        framework.SharedLister
    gangCache                   map[string]*Gang
}
```

当启动 kubernetes 调度器时，我们仅需要将我们当逻辑挂载到以下 4 个扩展点：
```go
var(
	_ framework.PreFilterPlugin = &GangScheduling{}
	_ framework.PostFilterPlugin = &GangScheduling{}
	_ framework.PermitPlugin = &GangScheduling{}
	_ framework.ReservePlugin = &Coscheduling{}
)
type GangScheduling interface{
    ActiveGang(pod *corev1.Pod, state *framework.CycleState)
    PreFilter(context.Context, *corev1.Pod) error
    PostFilter(ctx context.Context, state *CycleState, pod *v1.Pod, filteredNodeStatusMap NodeToStatusMap) (*PostFilterResult, *Status)
    Permit(context.Context, *corev1.Pod) Status
    Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string)
}
```
###### **PreFilter**

`NonStrict` 模式，我们仅处理 步骤一和二：

- 校验 Gang 下包含所有 pod 是否符合最小数，如果不符合则拒绝当前 pod。
- 校验 Gang 是否超时，如果超时则拒绝当前 pod。
- 校验 Gang scheduleCycleValid 字段是否为 true，如果为 false 则拒绝当前 pod。
- 尝试更新 `scheduleCycle`， `scheduleCycleValid`， `childrenScheduleRoundMap` 字段。


###### **PostFilter**

到达当前阶段说明 pod 没有通过 Filter 校验，操作如下：
- 如果 `Strict` 模式，设置 `scheduleCycleValid` 字段为 false，同时释放所有已经完成调度的 pod。
- 如果 `NonStrict` 模式则不做任何操作。

###### **Permit**

到达当前阶段说明 pod 已经通过 Filter 校验，调度器插件将会计算 GangGroup 下所有 Gang 已经完成调度 pod 数量是否满足 Gang 最小值。

- 如果 Gang 不符合 bind 条件，我们会将 pod 状态修改为 "Wait" 并配置超时时间，同时 bind 协程一直保持等待直到超时或者通过校验。
  随后，我们会执行 `ActiveGang` 操作，该操作会将归属于 Gang 的 pod 从 `schedulableQueue` 或者 `backoffQueue` 队列中迁移到 `activeQueue` 队列，
  如此操作之后，pod 将会被尽快尽享调度。

> 注意⚠️ ，社区调度器中，调度周期最长不能超过 15 分钟，我们则需要通过改写 RunPermitPlugins 将调度周期配置超过 15 分钟。

- 如果 Gang 符合 bind 条件，我们将等待中 pod 状态修改为 "Success"，此时 bind 协程将结束等待并执行后续操作，并将 Gang 对象中 `ResourceSatisfied` 设置为 true。

###### **Un-reserve**

如果 permit 阶段超时且 binding 阶段失败，此时调度阶段将会流转到 un-reserve 阶段，我们通过 Gang 对象中 `ResourceSatisfied` 值判断，如果此时值为 true 说明 binding 阶段失败，反之则说明 Gang 超时。

- 如果 permit 阶段超时，我们将在所有 Gang 下所有 pod annotation 中增加 `gang.scheduling.koordinator.sh/timeout=true`，同时释放所有已经调度成功的 pod。
  此时，Gang 下所有 pod 将永远不会再进行调度，用户需要手动处理 permit 超时问题。
- 如果 binding 阶段失败，Gang 资源累计操作将会结束，随后会回滚所有失败的 pod 。

###### **Init**

We will register pod's event handler to watch pod event for updating Gang.

## Unsolved Problems

## Alternatives
User can choose use Gang by `Strict` and `NonStrict` case by case.

## Implementation History

## References