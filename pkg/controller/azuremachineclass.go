/*
Copyright 2017 The Gardener Authors.

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

// Package controller is used to provide the core functionalities of machine-controller-manager
package controller

import (
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api"

	"github.com/golang/glog"

	"github.com/gardener/machine-controller-manager/pkg/apis/machine"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/validation"
)

// AzureMachineClassKind is used to identify the machineClassKind as Azure
const AzureMachineClassKind = "AzureMachineClass"

func (c *controller) machineDeploymentToAzureMachineClassDelete(obj interface{}) {
	machineDeployment, ok := obj.(*v1alpha1.MachineDeployment)
	if machineDeployment == nil || !ok {
		return
	}
	if machineDeployment.Spec.Template.Spec.Class.Kind == AzureMachineClassKind {
		c.azureMachineClassQueue.Add(machineDeployment.Spec.Template.Spec.Class.Name)
	}
}

func (c *controller) machineSetToAzureMachineClassDelete(obj interface{}) {
	machineSet, ok := obj.(*v1alpha1.MachineSet)
	if machineSet == nil || !ok {
		return
	}
	if machineSet.Spec.Template.Spec.Class.Kind == AzureMachineClassKind {
		c.azureMachineClassQueue.Add(machineSet.Spec.Template.Spec.Class.Name)
	}
}

func (c *controller) machineToAzureMachineClassDelete(obj interface{}) {
	machine, ok := obj.(*v1alpha1.Machine)
	if machine == nil || !ok {
		return
	}
	if machine.Spec.Class.Kind == AzureMachineClassKind {
		c.azureMachineClassQueue.Add(machine.Spec.Class.Name)
	}
}

func (c *controller) azureMachineClassAdd(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.azureMachineClassQueue.Add(key)
}

func (c *controller) azureMachineClassUpdate(oldObj, newObj interface{}) {
	old, ok := oldObj.(*v1alpha1.AzureMachineClass)
	if old == nil || !ok {
		return
	}
	new, ok := oldObj.(*v1alpha1.AzureMachineClass)
	if new == nil || !ok {
		return
	}

	c.azureMachineClassAdd(newObj)
}

// reconcileClusterAzureMachineClassKey reconciles an AzureMachineClass due to controller resync
// or an event on the azureMachineClass.
func (c *controller) reconcileClusterAzureMachineClassKey(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	class, err := c.azureMachineClassLister.AzureMachineClasses(c.namespace).Get(name)

	if errors.IsNotFound(err) {
		glog.Infof("%s %q: Not doing work because it has been deleted", AzureMachineClassKind, key)
		return nil
	}
	if err != nil {
		glog.Infof("%s %q: Unable to retrieve object from store: %v", AzureMachineClassKind, key, err)
		return err
	}

	return c.reconcileClusterAzureMachineClass(class)
}

func (c *controller) reconcileClusterAzureMachineClass(class *v1alpha1.AzureMachineClass) error {
	internalClass := &machine.AzureMachineClass{}
	err := api.Scheme.Convert(class, internalClass, nil)
	if err != nil {
		return err
	}
	// TODO this should be put in own API server
	validationerr := validation.ValidateAzureMachineClass(internalClass)
	if validationerr.ToAggregate() != nil && len(validationerr.ToAggregate().Errors()) > 0 {
		glog.V(2).Infof("Validation of %s failed %s", AzureMachineClassKind, validationerr.ToAggregate().Error())
		return nil
	}

	// Manipulate finalizers
	if class.DeletionTimestamp == nil {
		c.addAzureMachineClassFinalizers(class)
	}

	machines, err := c.findMachinesForClass(AzureMachineClassKind, class.Name)
	if err != nil {
		return err
	}

	if class.DeletionTimestamp != nil {
		if finalizers := sets.NewString(class.Finalizers...); !finalizers.Has(DeleteFinalizerName) {
			return nil
		}

		machineDeployments, err := c.findMachineDeploymentsForClass(AzureMachineClassKind, class.Name)
		if err != nil {
			return err
		}
		machineSets, err := c.findMachineSetsForClass(AzureMachineClassKind, class.Name)
		if err != nil {
			return err
		}
		if len(machineDeployments) == 0 && len(machineSets) == 0 && len(machines) == 0 {
			c.deleteAzureMachineClassFinalizers(class)
			return nil
		}

		glog.V(4).Infof("Cannot remove finalizer of %s because still Machine[s|Sets|Deployments] are referencing it", AzureMachineClassKind, class.Name)
		return nil
	}

	for _, machine := range machines {
		c.addMachine(machine)
	}
	return nil
}

/*
	SECTION
	Manipulate Finalizers
*/

func (c *controller) addAzureMachineClassFinalizers(class *v1alpha1.AzureMachineClass) {
	clone := class.DeepCopy()

	if finalizers := sets.NewString(clone.Finalizers...); !finalizers.Has(DeleteFinalizerName) {
		finalizers.Insert(DeleteFinalizerName)
		c.updateAzureMachineClassFinalizers(clone, finalizers.List())
	}
}

func (c *controller) deleteAzureMachineClassFinalizers(class *v1alpha1.AzureMachineClass) {
	clone := class.DeepCopy()

	if finalizers := sets.NewString(clone.Finalizers...); finalizers.Has(DeleteFinalizerName) {
		finalizers.Delete(DeleteFinalizerName)
		c.updateAzureMachineClassFinalizers(clone, finalizers.List())
	}
}

func (c *controller) updateAzureMachineClassFinalizers(class *v1alpha1.AzureMachineClass, finalizers []string) {
	// Get the latest version of the class so that we can avoid conflicts
	class, err := c.controlMachineClient.AzureMachineClasses(class.Namespace).Get(class.Name, metav1.GetOptions{})
	if err != nil {
		return
	}

	clone := class.DeepCopy()
	clone.Finalizers = finalizers
	_, err = c.controlMachineClient.AzureMachineClasses(class.Namespace).Update(clone)
	if err != nil {
		// Keep retrying until update goes through
		glog.Warning("Updated failed, retrying")
		c.updateAzureMachineClassFinalizers(class, finalizers)
	}
}