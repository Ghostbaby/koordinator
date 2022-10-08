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

package compatibledefaultpreemption

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/util/feature"
	scheduledconfigv1beta2config "k8s.io/kube-scheduler/config/v1beta2"
	"k8s.io/kubernetes/pkg/features"
	scheduledconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/v1beta2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	plfeature "k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

const (
	Name = "CompatibleDefaultPreemption"
)

type CompatibleDefaultPreemption struct {
	args *scheduledconfig.DefaultPreemptionArgs
	framework.PostFilterPlugin
}

func New(dpArgs runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	// 如果调度抢占启动参数为空，则生成缺省参数
	if dpArgs == nil {
		defaultPreemptionArgs, err := getDefaultPreemptionArgs()
		if err != nil {
			return nil, err
		}
		dpArgs = defaultPreemptionArgs
	} else {
		// 获取抢占参数
		unknownObj, ok := dpArgs.(*runtime.Unknown)
		if !ok {
			return nil, fmt.Errorf("got args of type %T, want *DefaultPreemptionArgs", dpArgs)
		}

		// 生成缺省参数
		defaultPreemptionArgs, err := getDefaultPreemptionArgs()
		if err != nil {
			return nil, err
		}

		// 使用当前配置覆盖缺省参数
		if err := frameworkruntime.DecodeInto(unknownObj, defaultPreemptionArgs); err != nil {
			return nil, err
		}
		dpArgs = defaultPreemptionArgs
	}

	// 此处主要用于兼容 kube 1.18~1.20 版本，需要关闭 EnablePodDisruptionBudget 特性
	fts := plfeature.Features{
		EnablePodAffinityNamespaceSelector: feature.DefaultFeatureGate.Enabled(features.PodAffinityNamespaceSelector),
		EnablePodOverhead:                  feature.DefaultFeatureGate.Enabled(features.PodOverhead),
		EnableReadWriteOncePod:             feature.DefaultFeatureGate.Enabled(features.ReadWriteOncePod),
		EnablePodDisruptionBudget:          false, // kube version <= 1.20 disable the feature
	}

	// 创建 kube-scheduler 默认抢占插件
	plg, err := defaultpreemption.New(dpArgs, fh, fts)
	if err != nil {
		return nil, err
	}
	return &CompatibleDefaultPreemption{
		args:             dpArgs.(*scheduledconfig.DefaultPreemptionArgs),
		PostFilterPlugin: plg.(framework.PostFilterPlugin),
	}, nil
}

func (plg *CompatibleDefaultPreemption) Name() string {
	return Name
}

// getDefaultPreemptionArgs 生成 kube-scheduler 默认调度抢占插件缺省启动参数
func getDefaultPreemptionArgs() (*scheduledconfig.DefaultPreemptionArgs, error) {
	var v1beta2args scheduledconfigv1beta2config.DefaultPreemptionArgs
	v1beta2.SetDefaults_DefaultPreemptionArgs(&v1beta2args)
	var defaultPreemptionArgs scheduledconfig.DefaultPreemptionArgs
	err := v1beta2.Convert_v1beta2_DefaultPreemptionArgs_To_config_DefaultPreemptionArgs(&v1beta2args, &defaultPreemptionArgs, nil)
	if err != nil {
		return nil, err
	}
	return &defaultPreemptionArgs, nil
}
