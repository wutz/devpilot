/*
Copyright 2026 wutz.

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

package controller

import (
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// Short aliases keep the call sites readable; they are constants, not state.
const (
	metav1DeleteBackground = metav1.DeletePropagationBackground
	metav1ConditionTrue    = metav1.ConditionTrue
	metav1ConditionFalse   = metav1.ConditionFalse
)

// ctrlSetOwner makes owner the controller of obj so the garbage collector
// releases the pod, the claim and their volumes together.
func ctrlSetOwner(obj client.Object, owner *devpilotv1alpha1.DevBox, scheme *runtime.Scheme) error {
	return controllerutil.SetControllerReference(owner, obj, scheme)
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

func isAlreadyExists(err error) bool {
	return apierrors.IsAlreadyExists(err)
}

func isConflict(err error) bool {
	return apierrors.IsConflict(err)
}

// timeNow is the single clock read by the controller.
func timeNow() time.Time {
	return time.Now().UTC()
}
