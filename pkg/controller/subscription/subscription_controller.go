// Copyright 2019 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package subscription

import (
	"context"
	"strings"
	"time"

	gerr "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	chnv1alpha1 "github.com/open-cluster-management/multicloud-operators-channel/pkg/apis/apps/v1"
	appv1alpha1 "github.com/open-cluster-management/multicloud-operators-subscription/pkg/apis/apps/v1"
	ghsub "github.com/open-cluster-management/multicloud-operators-subscription/pkg/subscriber/git"
	hrsub "github.com/open-cluster-management/multicloud-operators-subscription/pkg/subscriber/helmrepo"
	nssub "github.com/open-cluster-management/multicloud-operators-subscription/pkg/subscriber/namespace"
	ossub "github.com/open-cluster-management/multicloud-operators-subscription/pkg/subscriber/objectbucket"
	subutil "github.com/open-cluster-management/multicloud-operators-subscription/pkg/utils"

	"github.com/open-cluster-management/multicloud-operators-subscription/pkg/utils"
)

const (
	subscriptionActive string = "Active"
	subscriptionBlock  string = "Blocked"
)

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Subscription Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, hubconfig *rest.Config) error {
	hubclient, err := client.New(hubconfig, client.Options{})
	if err != nil {
		klog.Error("Failed to generate client to hub cluster with error:", err)
		return err
	}

	subs := make(map[string]appv1alpha1.Subscriber)

	if nssub.GetdefaultNsSubscriber() == nil {
		errmsg := "default namespace subscriber is not initialized"
		klog.Error(errmsg)

		return errors.NewServiceUnavailable(errmsg)
	}

	subs[chnv1alpha1.ChannelTypeNamespace] = nssub.GetdefaultNsSubscriber()
	subs[chnv1alpha1.ChannelTypeHelmRepo] = hrsub.GetDefaultSubscriber()
	subs[chnv1alpha1.ChannelTypeGitHub] = ghsub.GetDefaultSubscriber()
	subs[chnv1alpha1.ChannelTypeGit] = ghsub.GetDefaultSubscriber()
	subs[chnv1alpha1.ChannelTypeObjectBucket] = ossub.GetDefaultSubscriber()

	return add(mgr, newReconciler(mgr, hubclient, subs))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, hubclient client.Client, subscribers map[string]appv1alpha1.Subscriber) reconcile.Reconciler {
	rec := &ReconcileSubscription{
		Client:      mgr.GetClient(),
		scheme:      mgr.GetScheme(),
		hubclient:   hubclient,
		subscribers: subscribers,
		clk:         time.Now,
	}

	return rec
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("subscription-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Subscription
	err = c.Watch(&source.Kind{Type: &appv1alpha1.Subscription{}}, &handler.EnqueueRequestForObject{}, utils.SubscriptionPredicateFunctions)
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileSubscription implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileSubscription{}

type clock func() time.Time

// ReconcileSubscription reconciles a Subscription object
type ReconcileSubscription struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client.Client
	hubclient   client.Client
	scheme      *runtime.Scheme
	subscribers map[string]appv1alpha1.Subscriber
	clk         clock
}

// Reconcile reads that state of the cluster for a Subscription object and makes changes based on the state read
// and what is in the Subscription.Spec
func (r *ReconcileSubscription) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	klog.Info("Standalone/Endpoint Reconciling subscription: ", request.NamespacedName)

	instance := &appv1alpha1.Subscription{}
	err := r.Get(context.TODO(), request.NamespacedName, instance)

	if err != nil {
		if errors.IsNotFound(err) {
			klog.Info("Subscription: ", request.NamespacedName, " is gone")

			// Object not found, delete existing subscriberitem if any
			for _, sub := range r.subscribers {
				if err := sub.UnsubscribeItem(request.NamespacedName); err != nil {
					return reconcile.Result{RequeueAfter: time.Second * 2}, err
				}
			}

			objKind := schema.GroupVersionKind{Group: "", Kind: SecretKindStr, Version: "v1"}
			err := r.DeleteReferredObjects(request.NamespacedName, objKind)

			if err != nil {
				klog.Errorf("Had error %v while processing the referred secert", err)
			}

			objKind = schema.GroupVersionKind{Group: "", Kind: ConfigMapKindStr, Version: "v1"}
			err = r.DeleteReferredObjects(request.NamespacedName, objKind)

			if err != nil {
				klog.Errorf("Had error %v while processing the referred secert", err)
			}

			return reconcile.Result{}, err
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// if the subscription pause lable is true, stop subscription here.
	if subutil.GetPauseLabel(instance) {
		klog.Info("Subscription: ", request.NamespacedName, " is paused")
		return reconcile.Result{}, nil
	}

	pl := instance.Spec.Placement
	if pl != nil && pl.Local != nil && *pl.Local {
		reconcileErr := r.doReconcile(instance)

		instance.Status.Phase = appv1alpha1.SubscriptionSubscribed
		if reconcileErr != nil {
			instance.Status.Phase = appv1alpha1.SubscriptionFailed
			instance.Status.Reason = reconcileErr.Error()
			klog.Errorf("doReconcile got error %v", reconcileErr)
		}
	} else {
		// no longer local
		for _, sub := range r.subscribers {
			_ = sub.UnsubscribeItem(request.NamespacedName)
		}

		if instance.Status.Phase == appv1alpha1.SubscriptionFailed || instance.Status.Phase == appv1alpha1.SubscriptionSubscribed {
			instance.Status.Phase = ""
			instance.Status.Message = ""
			instance.Status.Reason = ""
		}

		if instance.Status.Statuses != nil {
			delete(instance.Status.Statuses, types.NamespacedName{}.String())
		}
	}

	instance.Status.LastUpdateTime = metav1.Now()

	// calculate the requeue time for updating the timewindow status
	nextStatusUpateAt := time.Duration(0)

	if instance.Spec.TimeWindow == nil {
		instance.Status.Message = subscriptionActive
	} else {
		if utils.IsInWindow(instance.Spec.TimeWindow, r.clk()) {
			instance.Status.Message = subscriptionActive
		} else {
			instance.Status.Message = subscriptionBlock
		}
		nextStatusUpateAt = utils.NextStatusReconcile(instance.Spec.TimeWindow, r.clk())
	}

	err = r.Status().Update(context.TODO(), instance)

	result := reconcile.Result{RequeueAfter: nextStatusUpateAt}

	if err != nil {
		klog.Errorf("failed to update status for subscription %v with error %v retry after 1 second", request.NamespacedName, err)

		result.RequeueAfter = 1 * time.Second
	}

	return result, err
}

func (r *ReconcileSubscription) doReconcile(instance *appv1alpha1.Subscription) error {
	var err error

	subitem := &appv1alpha1.SubscriberItem{}
	subitem.Subscription = instance

	subitem.Channel = &chnv1alpha1.Channel{}
	chnkey := utils.NamespacedNameFormat(instance.Spec.Channel)
	err = r.hubclient.Get(context.TODO(), chnkey, subitem.Channel)

	if err != nil {
		return gerr.Wrapf(err, "failed to get channel of subscription %v", instance)
	}

	if subitem.Channel.Spec.SecretRef != nil {
		subitem.ChannelSecret = &corev1.Secret{}
		chnseckey := types.NamespacedName{
			Name:      subitem.Channel.Spec.SecretRef.Name,
			Namespace: subitem.Channel.Namespace,
		}

		if err := r.hubclient.Get(context.TODO(), chnseckey, subitem.ChannelSecret); err != nil {
			return gerr.Wrap(err, "failed to get reference secret from channel")
		}
	}

	if subitem.Channel.Spec.ConfigMapRef != nil {
		subitem.ChannelConfigMap = &corev1.ConfigMap{}
		chncfgkey := types.NamespacedName{
			Name:      subitem.Channel.Spec.ConfigMapRef.Name,
			Namespace: subitem.Channel.Namespace,
		}

		if err := r.hubclient.Get(context.TODO(), chncfgkey, subitem.ChannelConfigMap); err != nil {
			return gerr.Wrap(err, "failed to get reference configmap from channel")
		}
	}

	if subitem.Channel.Spec.SecretRef != nil {
		obj := subitem.ChannelSecret

		gvk := schema.GroupVersionKind{Group: "", Kind: SecretKindStr, Version: "v1"}

		if err := r.ListAndDeployReferredObject(instance, gvk, obj); err != nil {
			return gerr.Wrapf(err, "Can't deploy reference secret %v for subscription %v", subitem.ChannelSecret.GetName(), instance.GetName())
		}
	}

	if subitem.Channel.Spec.ConfigMapRef != nil {
		obj := subitem.ChannelConfigMap
		gvk := schema.GroupVersionKind{Group: "", Kind: ConfigMapKindStr, Version: "v1"}
		err = r.ListAndDeployReferredObject(instance, gvk, obj)

		if err != nil {
			return gerr.Wrapf(err, "can't deploy reference configmap %v for subscription %v", obj.GetName(), instance.GetName())
		}
	}

	if instance.Spec.PackageFilter != nil && instance.Spec.PackageFilter.FilterRef != nil {
		subitem.SubscriptionConfigMap = &corev1.ConfigMap{}
		subcfgkey := types.NamespacedName{
			Name:      instance.Spec.PackageFilter.FilterRef.Name,
			Namespace: instance.Namespace,
		}

		errLocal := r.Client.Get(context.TODO(), subcfgkey, subitem.SubscriptionConfigMap)
		errRemote := r.hubclient.Get(context.TODO(), subcfgkey, subitem.SubscriptionConfigMap)

		if errRemote != nil && errLocal != nil {
			return gerr.Wrapf(err, "failed to get reference configMap %v of subsciption %v from hub", subcfgkey.String(), instance.GetName())
		}
	}

	subtype := strings.ToLower(string(subitem.Channel.Spec.Type))

	// subscribe it with right channel type and unsubscribe from other channel types (in case user modify channel type)
	for k, sub := range r.subscribers {
		if k != subtype {
			if err := sub.UnsubscribeItem(types.NamespacedName{Name: subitem.Subscription.Name, Namespace: subitem.Subscription.Namespace}); err != nil {
				klog.Errorf("failed to unsubscribe with subscriber %v error %+v", k, err)
			}
		}
	}

	if sub, ok := r.subscribers[subtype]; ok {
		if err := sub.SubscribeItem(subitem); err != nil {
			klog.Errorf("failed to subscribe with subscriber %v, error %+v", subtype, err)
		}
	}

	return nil
}
