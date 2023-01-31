package controllers

import (
	"context"

	"github.com/go-logr/logr"
	ingressv1alpha1 "github.com/ngrok/kubernetes-ingress-controller/api/v1alpha1"
	internalerrors "github.com/ngrok/kubernetes-ingress-controller/internal/errors"
	"github.com/ngrok/kubernetes-ingress-controller/internal/store"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// This implements the Reconciler for the controller-runtime
// https://pkg.go.dev/sigs.k8s.io/controller-runtime#section-readme
type IngressReconciler struct {
	client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Namespace string
	Driver    *store.Driver
}

// Create a new controller using our reconciler and set it up with the manager
func (irec *IngressReconciler) SetupWithManager(mgr ctrl.Manager, d *store.Driver) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		// TODO:(initial-store): Watch ingress classes and create a basic function to find all ings for thats class
		Watches(
			&source.Kind{Type: &ingressv1alpha1.Domain{}},
			store.NewEnqueueOwnersAfterSyncing(d, mgr.GetClient()),
		).
		Watches(
			&source.Kind{Type: &ingressv1alpha1.HTTPSEdge{}},
			store.NewEnqueueOwnersAfterSyncing(d, mgr.GetClient()),
		).
		Watches(
			&source.Kind{Type: &ingressv1alpha1.Tunnel{}},
			store.NewEnqueueOwnersAfterSyncing(d, mgr.GetClient()),
		).
		Complete(irec)
}

// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="networking.k8s.io",resources=ingresses,verbs=get;list;watch;update
// +kubebuilder:rbac:groups="networking.k8s.io",resources=ingresses/status,verbs=get;list;watch;update
// +kubebuilder:rbac:groups="networking.k8s.io",resources=ingressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// This reconcile function is called by the controller-runtime manager.
// It is invoked whenever there is an event that occurs for a resource
// being watched (in our case, ingress objects). If you tail the controller
// logs and delete, update, edit ingress objects, you see the events come in.
func (irec *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := irec.Log.WithValues("ingress", req.NamespacedName)
	ctx = ctrl.LoggerInto(ctx, log)
	ingress := &netv1.Ingress{}
	err := irec.Client.Get(ctx, req.NamespacedName, ingress)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			// If its fully gone, delete it from the store
			if err := irec.Driver.DeleteIngress(req.NamespacedName); err != nil {
				log.Error(err, "Failed to delete ingress from store")
				return ctrl.Result{}, err
			}

			err = irec.Driver.Sync(ctx, irec.Client)
			if err != nil {
				log.Error(err, "Failed to sync ingress to store")
				return ctrl.Result{}, err
			}

		}
		return ctrl.Result{}, err // Otherwise, its a real error
	}

	// Ensure the ingress object is up to date in the store
	err = irec.Driver.Update(ingress)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Even though we already have the ingress object, leverage the store to ensure this works off the same data as everything else
	ingress, err = irec.Driver.Store.GetNgrokIngressV1(ingress.Name, ingress.Namespace)
	if internalerrors.IsErrDifferentIngressClass(err) {
		log.Info("Ingress is not of type ngrok so skipping it")
		return ctrl.Result{}, nil
	}
	if internalerrors.IsErrInvalidIngressSpec(err) {
		log.Info("Ingress is not valid so skipping it")
		return ctrl.Result{}, nil
	}
	if err != nil {
		log.Error(err, "Failed to get ingress from store")
		return ctrl.Result{}, err
	}

	if ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so register and sync finalizer
		if err := registerAndSyncFinalizer(ctx, irec.Client, ingress); err != nil {
			log.Error(err, "Failed to register finalizer")
			return ctrl.Result{}, err
		}
	} else {
		if hasFinalizer(ingress) {
			log.Info("Deleting ingress from store")
			if err := irec.delete(ctx, ingress); err != nil {
				log.Error(err, "Failed to delete ingress")
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	return irec.reconcileAll(ctx, ingress)
}

// Delete is called when the ingress object is being deleted
func (irec *IngressReconciler) delete(ctx context.Context, ingress *netv1.Ingress) error {
	if err := removeAndSyncFinalizer(ctx, irec.Client, ingress); err != nil {
		irec.Log.Error(err, "Failed to remove finalizer")
		return err
	}
	// Remove the ingress object from the store
	return irec.Driver.Delete(ingress)
}

func (irec *IngressReconciler) reconcileAll(ctx context.Context, ingress *netv1.Ingress) (reconcile.Result, error) {
	log := irec.Log
	// First Update the store
	err := irec.Driver.Update(ingress)
	if err != nil {
		log.Error(err, "Failed to add ingress to store")
		return ctrl.Result{}, err
	}

	err = irec.Driver.Sync(ctx, irec.Client)
	if err != nil {
		log.Error(err, "Failed to sync ingress to store")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
