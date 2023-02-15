/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayapi "sigs.k8s.io/gateway-api/apis/v1beta1"

	cgcapi "github.com/tv2/cloud-gateway-controller/apis/cgc.tv2.dk/v1alpha1"
	selfapi "github.com/tv2/cloud-gateway-controller/pkg/api"
)

type HTTPRouteReconciler struct {
	client    client.Client
	scheme    *runtime.Scheme
	dynClient dynamic.Interface
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update

func (r *HTTPRouteReconciler) Client() client.Client {
	return r.client
}

func (r *HTTPRouteReconciler) Scheme() *runtime.Scheme {
	return r.scheme
}

func (r *HTTPRouteReconciler) DynamicClient() dynamic.Interface {
	return r.dynClient
}

func NewHTTPRouteController(mgr ctrl.Manager, config *rest.Config) *HTTPRouteReconciler {
	r := &HTTPRouteReconciler{
		client:    mgr.GetClient(),
		scheme:    mgr.GetScheme(),
		dynClient: dynamic.NewForConfigOrDie(config),
	}
	return r
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayapi.HTTPRoute{}).
		Complete(r)
}

func lookupParent(ctx context.Context, r ControllerClient, rt *gatewayapi.HTTPRoute, p gatewayapi.ParentReference) (*gatewayapi.Gateway, error) {
	if p.Namespace == nil {
		return lookupGateway(ctx, r, p.Name, rt.ObjectMeta.Namespace)
	}
	return lookupGateway(ctx, r, p.Name, string(*p.Namespace))
}

func findParentRouteStatus(rtStatus *gatewayapi.RouteStatus, parent gatewayapi.ParentReference) *gatewayapi.RouteParentStatus {
	for i := range rtStatus.Parents {
		pStat := &rtStatus.Parents[i]
		if pStat.ParentRef == parent && pStat.ControllerName == selfapi.SelfControllerName {
			return pStat
		}
	}
	return nil
}

func setRouteStatusCondition(rtStatus *gatewayapi.RouteStatus, parent gatewayapi.ParentReference, newCondition *metav1.Condition) {
	if newCondition.LastTransitionTime.IsZero() {
		newCondition.LastTransitionTime = metav1.NewTime(time.Now())
	}

	existingParentRouteStat := findParentRouteStatus(rtStatus, parent)
	if existingParentRouteStat == nil {
		newStatus := gatewayapi.RouteParentStatus{
			ParentRef:      parent,
			ControllerName: selfapi.SelfControllerName,
			Conditions:     []metav1.Condition{*newCondition},
		}
		rtStatus.Parents = append(rtStatus.Parents, newStatus)
		return
	}

	meta.SetStatusCondition(&existingParentRouteStat.Conditions, *newCondition)
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var rt gatewayapi.HTTPRoute
	if err := r.Client().Get(ctx, req.NamespacedName, &rt); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("HTTPRoute")

	prefs := rt.Spec.CommonRouteSpec.ParentRefs
	// FIXME check kind of parent ref is Gateway and missing parentRef. Accepts more than one parent ref
	pref := prefs[0]

	// Spec says: 'When unspecified, this refers to the local namespace of the Route.'
	var ns string
	if pref.Namespace == nil {
		ns = rt.ObjectMeta.Namespace
	} else {
		ns = string(*pref.Namespace)
	}
	gw := &gatewayapi.Gateway{}
	if err := r.Client().Get(ctx, types.NamespacedName{Name: string(pref.Name), Namespace: ns}, gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("reconcile", "gateway", gw)

	gwc, err := lookupGatewayClass(ctx, r, gw.Spec.GatewayClassName)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !isOurGatewayClass(gwc) {
		return ctrl.Result{}, nil
	}

	gcp, err := lookupGatewayClassParameters(ctx, r, gwc)
	if err != nil {
		return ctrl.Result{RequeueAfter: dependencyMissingRequeuePeriod}, fmt.Errorf("parameters for GatewayClass %q not found: %w", gwc.ObjectMeta.Name, err)
	}

	if err := applyHTTPRouteTemplates(ctx, r, &rt, gcp); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to apply templates: %w", err)
	}

	doStatusUpdate := false
	rt.Status.Parents = []gatewayapi.RouteParentStatus{}
	for _, parent := range rt.Spec.ParentRefs {
		if parent.Namespace == nil {
			// Route parents default to same namespace as route
			parent.Namespace = (*gatewayapi.Namespace)(&rt.ObjectMeta.Namespace)
		}
		gw, err := lookupParent(ctx, r, &rt, parent)
		if err != nil {
			continue
		}
		gwcRef, err := lookupGatewayClass(ctx, r, gw.Spec.GatewayClassName)
		if err != nil || !isOurGatewayClass(gwcRef) {
			continue
		}
		doStatusUpdate = true

		setRouteStatusCondition(&rt.Status.RouteStatus, parent,
			&metav1.Condition{
				Type:   string(gatewayapi.RouteConditionAccepted),
				Status: "True",
				Reason: string(gatewayapi.RouteReasonAccepted),
			})
	}

	if doStatusUpdate {
		if err := r.Client().Status().Update(ctx, &rt); err != nil {
			logger.Error(err, "unable to update HTTPRoute status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// Parameters used to render HTTPRoute templates
type httprouteTemplateValues struct {
	// Parent HTTPRoute
	HTTPRoute *gatewayapi.HTTPRoute
}

func applyHTTPRouteTemplates(ctx context.Context, r ControllerDynClient, rtParent *gatewayapi.HTTPRoute, params *cgcapi.GatewayClassParameters) error {
	templateValues := httprouteTemplateValues{
		HTTPRoute: rtParent,
	}
	for tmplKey, tmpl := range params.Spec.HTTPRouteTemplate.ResourceTemplates {
		u, err := template2Unstructured(tmpl, &templateValues)
		if err != nil {
			return fmt.Errorf("cannot render template %q: %w", tmplKey, err)
		}

		if err := ctrl.SetControllerReference(rtParent, u, r.Scheme()); err != nil {
			return fmt.Errorf("cannot set owner for resource created from template %q: %w", tmplKey, err)
		}

		if err := patchUnstructured(ctx, r, u, rtParent.ObjectMeta.Namespace); err != nil {
			return fmt.Errorf("cannot apply template %q: %w", tmplKey, err)
		}
	}
	return nil
}
