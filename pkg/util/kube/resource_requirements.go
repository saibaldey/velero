/*
Copyright 2019 the Velero contributors.

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

package kube

import (
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseResourceRequirements takes a set of CPU and memory requests and limit string
// values and returns a ResourceRequirements struct to be used in a Container.
// An error is returned if we cannot parse the request/limit.
func ParseResourceRequirements(cpuRequest, memRequest, cpuLimit, memLimit string) (corev1.ResourceRequirements, error) {
	var resources corev1.ResourceRequirements

	parsedCPURequest, err := resource.ParseQuantity(cpuRequest)
	if err != nil {
		return resources, errors.Wrapf(err, `couldn't parse CPU request "%s"`, cpuRequest)
	}

	parsedMemRequest, err := resource.ParseQuantity(memRequest)
	if err != nil {
		return resources, errors.Wrapf(err, `couldn't parse memory request "%s"`, memRequest)
	}

	parsedCPULimit, err := resource.ParseQuantity(cpuLimit)
	if err != nil {
		return resources, errors.Wrapf(err, `couldn't parse CPU limit "%s"`, cpuLimit)
	}

	parsedMemLimit, err := resource.ParseQuantity(memLimit)
	if err != nil {
		return resources, errors.Wrapf(err, `couldn't parse memory limit "%s"`, memLimit)
	}

	if parsedCPURequest.Cmp(parsedCPULimit) > 0 {
		return resources, errors.WithStack(errors.Errorf(`CPU request "%s" must be less than or equal to CPU limit "%s"`, cpuRequest, cpuLimit))
	}

	if parsedMemRequest.Cmp(parsedMemLimit) > 0 {
		return resources, errors.WithStack(errors.Errorf(`Memory request "%s" must be less than or equal to Memory limit "%s"`, memRequest, memLimit))
	}

	resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    parsedCPURequest,
		corev1.ResourceMemory: parsedMemRequest,
	}
	resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU:    parsedCPULimit,
		corev1.ResourceMemory: parsedMemLimit,
	}

	return resources, nil
}
