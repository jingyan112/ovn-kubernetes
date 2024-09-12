package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1infomer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	netv1lister "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnapplyconfkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	userdefinednetworkclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	userdefinednetworkscheme "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/scheme"
	userdefinednetworkinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"

	nadnotifier "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/notifier"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type RenderNetAttachDefManifest func(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error)

type Controller struct {
	Controller controller.Controller

	udnClient userdefinednetworkclientset.Interface
	udnLister userdefinednetworklister.UserDefinedNetworkLister

	nadNotifier *nadnotifier.NetAttachDefNotifier
	nadClient   netv1clientset.Interface
	nadLister   netv1lister.NetworkAttachmentDefinitionLister

	renderNadFn RenderNetAttachDefManifest

	podInformer corev1informer.PodInformer

	networkInUseRequeueInterval time.Duration
	eventRecorder               record.EventRecorder
}

const defaultNetworkInUseCheckInterval = 1 * time.Minute

func New(
	nadClient netv1clientset.Interface,
	nadInfomer netv1infomer.NetworkAttachmentDefinitionInformer,
	udnClient userdefinednetworkclientset.Interface,
	udnInformer userdefinednetworkinformer.UserDefinedNetworkInformer,
	renderNadFn RenderNetAttachDefManifest,
	podInformer corev1informer.PodInformer,
	eventRecorder record.EventRecorder,
) *Controller {
	udnLister := udnInformer.Lister()
	c := &Controller{
		nadClient:                   nadClient,
		nadLister:                   nadInfomer.Lister(),
		udnClient:                   udnClient,
		udnLister:                   udnLister,
		renderNadFn:                 renderNadFn,
		podInformer:                 podInformer,
		networkInUseRequeueInterval: defaultNetworkInUseCheckInterval,
		eventRecorder:               eventRecorder,
	}
	cfg := &controller.ControllerConfig[userdefinednetworkv1.UserDefinedNetwork]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcile,
		ObjNeedsUpdate: c.udnNeedUpdate,
		Threadiness:    1,
		Informer:       udnInformer.Informer(),
		Lister:         udnLister.List,
	}
	c.Controller = controller.NewController[userdefinednetworkv1.UserDefinedNetwork]("user-defined-network-controller", cfg)

	c.nadNotifier = nadnotifier.NewNetAttachDefNotifier(nadInfomer, c)

	return c
}

func (c *Controller) ReconcileNetAttachDef(key string) {
	// enqueue network-attachment-definitions requests in the controller workqueue
	c.Controller.Reconcile(key)
}

func (c *Controller) Run() error {
	klog.Infof("Starting UserDefinedNetworkManager Controllers")
	if err := controller.Start(c.nadNotifier.Controller, c.Controller); err != nil {
		return fmt.Errorf("unable to start UserDefinedNetworkManager controller: %v", err)
	}

	return nil
}

func (c *Controller) Shutdown() {
	controller.Stop(c.nadNotifier.Controller, c.Controller)
}

func (c *Controller) udnNeedUpdate(_, _ *userdefinednetworkv1.UserDefinedNetwork) bool {
	return true
}

// reconcile get the user-defined-network CRD instance key and reconcile it according to spec.
// It creates network-attachment-definition according to spec at the namespace the UDN object resides.
// The NAD object are created with the same key as the request NAD, having both kinds have the same key enable
// the controller to act on NAD changes as well and reconciles NAD objects (e.g: in case NAD is deleted it will be re-created).
func (c *Controller) reconcile(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	udn, err := c.udnLister.UserDefinedNetworks(namespace).Get(name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to get UserDefinedNetwork %q from cache: %v", key, err)
	}

	udnCopy := udn.DeepCopy()

	nadCopy, syncErr := c.syncUserDefinedNetwork(udnCopy)

	updateStatusErr := c.updateUserDefinedNetworkStatus(udnCopy, nadCopy, syncErr)

	var networkInUse *networkInUseError
	if errors.As(syncErr, &networkInUse) {
		c.Controller.ReconcileAfter(key, c.networkInUseRequeueInterval)
		return updateStatusErr
	}

	return errors.Join(syncErr, updateStatusErr)
}

type networkInUseError struct {
	err error
}

func (n *networkInUseError) Error() string {
	return n.err.Error()
}

func (c *Controller) syncUserDefinedNetwork(udn *userdefinednetworkv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	if udn == nil {
		return nil, nil
	}

	if !udn.DeletionTimestamp.IsZero() { // udn is being  deleted
		if controllerutil.ContainsFinalizer(udn, template.FinalizerUserDefinedNetwork) {
			if err := c.deleteNAD(udn, udn.Namespace); err != nil {
				return nil, fmt.Errorf("failed to delete NetworkAttachmentDefinition [%s/%s]: %w", udn.Namespace, udn.Name, err)
			}

			controllerutil.RemoveFinalizer(udn, template.FinalizerUserDefinedNetwork)
			udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to remove finalizer to UserDefinedNetwork: %w", err)
			}
			klog.Infof("Finalizer removed from UserDefinedNetworks [%s/%s]", udn.Namespace, udn.Name)
		}

		return nil, nil
	}

	if finalizerAdded := controllerutil.AddFinalizer(udn, template.FinalizerUserDefinedNetwork); finalizerAdded {
		udn, err := c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to add finalizer to UserDefinedNetwork: %w", err)
		}
		klog.Infof("Added Finalizer to UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
	}

	return c.updateNAD(udn, udn.Namespace)
}

func (c *Controller) updateUserDefinedNetworkStatus(udn *userdefinednetworkv1.UserDefinedNetwork, nad *netv1.NetworkAttachmentDefinition, syncError error) error {
	if udn == nil {
		return nil
	}

	networkReadyCondition := newNetworkReadyCondition(nad, syncError)

	conditions, updated := updateCondition(udn.Status.Conditions, networkReadyCondition)

	if updated {
		var err error
		conditionsApply := make([]*metaapplyv1.ConditionApplyConfiguration, len(conditions))
		for i := range conditions {
			conditionsApply[i] = &metaapplyv1.ConditionApplyConfiguration{
				Type:               &conditions[i].Type,
				Status:             &conditions[i].Status,
				LastTransitionTime: &conditions[i].LastTransitionTime,
				Reason:             &conditions[i].Reason,
				Message:            &conditions[i].Message,
			}
		}
		udnApplyConf := udnapplyconfkv1.UserDefinedNetwork(udn.Name, udn.Namespace).
			WithStatus(udnapplyconfkv1.UserDefinedNetworkStatus().
				WithConditions(conditionsApply...))
		opts := metav1.ApplyOptions{FieldManager: "user-defined-network-controller"}
		udn, err = c.udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).ApplyStatus(context.Background(), udnApplyConf, opts)
		if err != nil {
			if kerrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to update UserDefinedNetwork status: %w", err)
		}
		klog.Infof("Updated status UserDefinedNetwork [%s/%s]", udn.Namespace, udn.Name)
	}

	return nil
}

func newNetworkReadyCondition(nad *netv1.NetworkAttachmentDefinition, syncError error) *metav1.Condition {
	now := metav1.Now()
	networkReadyCondition := &metav1.Condition{
		Type:               "NetworkReady",
		Status:             metav1.ConditionTrue,
		Reason:             "NetworkAttachmentDefinitionReady",
		Message:            "NetworkAttachmentDefinition has been created",
		LastTransitionTime: now,
	}

	if nad != nil && !nad.DeletionTimestamp.IsZero() {
		networkReadyCondition.Status = metav1.ConditionFalse
		networkReadyCondition.Reason = "NetworkAttachmentDefinitionDeleted"
		networkReadyCondition.Message = "NetworkAttachmentDefinition is being deleted"
	}
	if syncError != nil {
		networkReadyCondition.Status = metav1.ConditionFalse
		networkReadyCondition.Reason = "SyncError"
		networkReadyCondition.Message = syncError.Error()
	}

	return networkReadyCondition
}

func updateCondition(conditions []metav1.Condition, cond *metav1.Condition) ([]metav1.Condition, bool) {
	if len(conditions) == 0 {
		return append(conditions, *cond), true
	}

	idx := slices.IndexFunc(conditions, func(c metav1.Condition) bool {
		return (c.Type == cond.Type) &&
			(c.Status != cond.Status || c.Reason != cond.Reason || c.Message != cond.Message)
	})
	if idx != -1 {
		return slices.Replace(conditions, idx, idx+1, *cond), true
	}
	return conditions, false
}

// UpdateSubsystemCondition may be used by other controllers handling UDN/NAD/network setup to report conditions that
// may affect UDN functionality.
// FieldManager should be unique for every subsystem.
// If given network is not managed by a UDN, no condition will be reported and no error will be returned.
// Events may be used to report additional information about the condition to avoid overloading the condition message.
// When condition should not change, but new events should be reported, pass condition = nil.
func (c *Controller) UpdateSubsystemCondition(networkName string, fieldManager string, condition *metav1.Condition,
	events ...*util.EventDetails) error {
	// try to find udn using network name
	udnNamespace, udnName := template.ParseNetworkName(networkName)
	if udnName == "" {
		return nil
	}
	udn, err := c.udnLister.UserDefinedNetworks(udnNamespace).Get(udnName)
	if err != nil {
		return nil
	}

	udnRef, err := reference.GetReference(userdefinednetworkscheme.Scheme, udn)
	if err != nil {
		return fmt.Errorf("failed to get object reference for UserDefinedNetwork %s/%s: %w", udnNamespace, udnName, err)
	}
	for _, event := range events {
		c.eventRecorder.Event(udnRef, event.EventType, event.Reason, event.Note)
	}

	if condition == nil {
		return nil
	}

	applyCondition := &metaapplyv1.ConditionApplyConfiguration{
		Type:               &condition.Type,
		Status:             &condition.Status,
		LastTransitionTime: &condition.LastTransitionTime,
		Reason:             &condition.Reason,
		Message:            &condition.Message,
	}

	udnStatus := udnapplyconfkv1.UserDefinedNetworkStatus().WithConditions(applyCondition)

	applyUDN := udnapplyconfkv1.UserDefinedNetwork(udnName, udnNamespace).WithStatus(udnStatus)
	opts := metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}
	_, err = c.udnClient.K8sV1().UserDefinedNetworks(udnNamespace).ApplyStatus(context.Background(), applyUDN, opts)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to update UserDefinedNetwork %s/%s status: %w", udnNamespace, udnName, err)
	}
	return nil
}
