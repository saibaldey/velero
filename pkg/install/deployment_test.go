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

package install

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestDeployment(t *testing.T) {
	deploy := Deployment("velero")

	assert.Equal(t, "velero", deploy.ObjectMeta.Namespace)

	deploy = Deployment("velero", WithRestoreOnly())
	assert.Equal(t, "--restore-only", deploy.Spec.Template.Spec.Containers[0].Args[1])

	deploy = Deployment("velero", WithEnvFromSecretKey("my-var", "my-secret", "my-key"))
	envSecret := deploy.Spec.Template.Spec.Containers[0].Env[1]
	assert.Equal(t, "my-var", envSecret.Name)
	assert.Equal(t, "my-secret", envSecret.ValueFrom.SecretKeyRef.LocalObjectReference.Name)
	assert.Equal(t, "my-key", envSecret.ValueFrom.SecretKeyRef.Key)

	deploy = Deployment("velero", WithImage("gcr.io/heptio-images/velero:v0.11"))
	assert.Equal(t, "gcr.io/heptio-images/velero:v0.11", deploy.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, corev1.PullIfNotPresent, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy)

	deploy = Deployment("velero", WithSecret(true))
	assert.Equal(t, 4, len(deploy.Spec.Template.Spec.Containers[0].Env))
	assert.Equal(t, 3, len(deploy.Spec.Template.Spec.Volumes))
}
