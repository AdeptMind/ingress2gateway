/*
Copyright © 2022 Kubernetes Authors

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
package i2gw

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

var (
	gatewayGVK = schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1beta1",
		Kind:    "Gateway",
	}

	httpRouteGVK = schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1beta1",
		Kind:    "HTTPRoute",
	}
)

type ruleGroupKey string

type ingressAggregator struct {
	ruleGroups      map[ruleGroupKey]*ingressRuleGroup
	defaultBackends []ingressDefaultBackend
}

type pathMatchKey string

type ingressRuleGroup struct {
	namespace    string
	ingressClass string
	host         string
	tls          []networkingv1.IngressTLS
	rules        []ingressRule
}

type ingressRule struct {
	rule  networkingv1.IngressRule
	extra *extra
}

type ingressDefaultBackend struct {
	name         string
	namespace    string
	ingressClass string
	backend      networkingv1.IngressBackend
}

type ingressPath struct {
	path  networkingv1.HTTPIngressPath
	extra *extra
}

type extra struct {
	canary *canary
}

type canary struct {
	enable           bool
	headerKey        string
	headerValue      string
	headerRegexMatch bool
	weight           int
	weightTotal      int
}

func (a *ingressAggregator) addIngress(ingress networkingv1.Ingress) {
	var ingressClass string
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		ingressClass = *ingress.Spec.IngressClassName
	} else if _, ok := ingress.Annotations[networkingv1beta1.AnnotationIngressClass]; ok {
		ingressClass = ingress.Annotations[networkingv1beta1.AnnotationIngressClass]
	} else {
		ingressClass = ingress.Name
	}
	e := getExtra(ingress)
	for _, rule := range ingress.Spec.Rules {
		a.addIngressRule(ingress.Namespace, ingressClass, rule, ingress.Spec, e)
	}
	if ingress.Spec.DefaultBackend != nil {
		a.defaultBackends = append(a.defaultBackends, ingressDefaultBackend{
			name:         ingress.Name,
			namespace:    ingress.Namespace,
			ingressClass: ingressClass,
			backend:      *ingress.Spec.DefaultBackend,
		})
	}
}

func (a *ingressAggregator) addIngressRule(namespace, ingressClass string, rule networkingv1.IngressRule, iSpec networkingv1.IngressSpec, e *extra) {
	rgKey := ruleGroupKey(fmt.Sprintf("%s/%s/%s", namespace, ingressClass, rule.Host))
	rg, ok := a.ruleGroups[rgKey]
	if !ok {
		rg = &ingressRuleGroup{
			namespace:    namespace,
			ingressClass: ingressClass,
			host:         rule.Host,
		}
		a.ruleGroups[rgKey] = rg
	}
	if len(iSpec.TLS) > 0 {
		rg.tls = append(rg.tls, iSpec.TLS...)
	}
	rg.rules = append(rg.rules, ingressRule{rule: rule, extra: e})
}

func (a *ingressAggregator) toHTTPRoutesAndGateways() ([]gatewayv1beta1.HTTPRoute, []gatewayv1beta1.Gateway, []error) {
	var httpRoutes []gatewayv1beta1.HTTPRoute
	var errors []error
	listenersByNamespacedGateway := map[string][]gatewayv1beta1.Listener{}

	for _, rg := range a.ruleGroups {
		listener := gatewayv1beta1.Listener{}
		if rg.host != "" {
			listener.Hostname = (*gatewayv1beta1.Hostname)(&rg.host)
		} else if len(rg.tls) == 1 && len(rg.tls[0].Hosts) == 1 {
			listener.Hostname = (*gatewayv1beta1.Hostname)(&rg.tls[0].Hosts[0])
		}
		if len(rg.tls) > 0 {
			listener.TLS = &gatewayv1beta1.GatewayTLSConfig{}
		}
		for _, tls := range rg.tls {
			listener.TLS.CertificateRefs = append(listener.TLS.CertificateRefs,
				gatewayv1beta1.SecretObjectReference{Name: gatewayv1beta1.ObjectName(tls.SecretName)})
		}
		gwKey := fmt.Sprintf("%s/%s", rg.namespace, rg.ingressClass)
		listenersByNamespacedGateway[gwKey] = append(listenersByNamespacedGateway[gwKey], listener)
		httpRoute, errors := rg.toHTTPRoute()
		httpRoutes = append(httpRoutes, httpRoute)
		errors = append(errors, errors...)
	}

	for _, db := range a.defaultBackends {
		httpRoute := gatewayv1beta1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-default-backend", db.name),
				Namespace: db.namespace,
			},
			Spec: gatewayv1beta1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1beta1.CommonRouteSpec{
					ParentRefs: []gatewayv1beta1.ParentReference{{
						Name: gatewayv1beta1.ObjectName(db.ingressClass),
					}},
				},
			},
			Status: gatewayv1beta1.HTTPRouteStatus{
				RouteStatus: gatewayv1beta1.RouteStatus{
					Parents: []gatewayv1beta1.RouteParentStatus{},
				},
			},
		}
		httpRoute.SetGroupVersionKind(httpRouteGVK)

		backendRef, err := toBackendRef(db.backend)
		if err != nil {
			errors = append(errors, err)
		} else {
			httpRoute.Spec.Rules = append(httpRoute.Spec.Rules, gatewayv1beta1.HTTPRouteRule{
				BackendRefs: []gatewayv1beta1.HTTPBackendRef{{BackendRef: *backendRef}},
			})
		}

		httpRoutes = append(httpRoutes, httpRoute)
	}

	gatewaysByKey := map[string]*gatewayv1beta1.Gateway{}
	for gwKey, listeners := range listenersByNamespacedGateway {
		parts := strings.Split(gwKey, "/")
		if len(parts) != 2 {
			errors = append(errors, fmt.Errorf("Error generating Gateway listeners for key: %s", gwKey))
			continue
		}
		gateway := gatewaysByKey[gwKey]
		if gateway == nil {
			gateway = &gatewayv1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: parts[0],
					Name:      parts[1],
				},
				Spec: gatewayv1beta1.GatewaySpec{
					GatewayClassName: gatewayv1beta1.ObjectName(parts[1]),
				},
			}
			gateway.SetGroupVersionKind(gatewayGVK)
			gatewaysByKey[gwKey] = gateway
		}
		for _, listener := range listeners {
			var listenerNamePrefix string
			if listener.Hostname != nil && *listener.Hostname != "" {
				listenerNamePrefix = fmt.Sprintf("%s-", nameFromHost(string(*listener.Hostname)))
			}

			gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1beta1.Listener{
				Name:     gatewayv1beta1.SectionName(fmt.Sprintf("%shttp", listenerNamePrefix)),
				Hostname: listener.Hostname,
				Port:     80,
				Protocol: gatewayv1beta1.HTTPProtocolType,
			})
			if listener.TLS != nil {
				gateway.Spec.Listeners = append(gateway.Spec.Listeners, gatewayv1beta1.Listener{
					Name:     gatewayv1beta1.SectionName(fmt.Sprintf("%shttps", listenerNamePrefix)),
					Hostname: listener.Hostname,
					Port:     443,
					Protocol: gatewayv1beta1.HTTPSProtocolType,
					TLS:      listener.TLS,
				})
			}
		}
	}

	var gateways []gatewayv1beta1.Gateway
	for _, gw := range gatewaysByKey {
		gateways = append(gateways, *gw)
	}

	return httpRoutes, gateways, errors
}

func (rg *ingressRuleGroup) toHTTPRoute() (gatewayv1beta1.HTTPRoute, []error) {
	pathsByMatchGroup := map[pathMatchKey][]ingressPath{}
	errors := []error{}

	for _, ir := range rg.rules {
		for _, path := range ir.rule.HTTP.Paths {
			ip := ingressPath{path: path, extra: ir.extra}
			pmKey := getPathMatchKey(ip)
			pathsByMatchGroup[pmKey] = append(pathsByMatchGroup[pmKey], ip)
		}
	}

	httpRoute := gatewayv1beta1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nameFromHost(rg.host),
			Namespace: rg.namespace,
		},
		Spec: gatewayv1beta1.HTTPRouteSpec{},
		Status: gatewayv1beta1.HTTPRouteStatus{
			RouteStatus: gatewayv1beta1.RouteStatus{
				Parents: []gatewayv1beta1.RouteParentStatus{},
			},
		},
	}
	httpRoute.SetGroupVersionKind(httpRouteGVK)

	if rg.ingressClass != "" {
		httpRoute.Spec.ParentRefs = []gatewayv1beta1.ParentReference{{Name: gatewayv1beta1.ObjectName(rg.ingressClass)}}
	}
	if rg.host != "" {
		httpRoute.Spec.Hostnames = []gatewayv1beta1.Hostname{gatewayv1beta1.Hostname(rg.host)}
	}

	for _, paths := range pathsByMatchGroup {
		match, err := toHTTPRouteMatch(paths[0])
		if err != nil {
			errors = append(errors, err)
			continue
		}
		hrRule := gatewayv1beta1.HTTPRouteRule{
			Matches: []gatewayv1beta1.HTTPRouteMatch{*match},
		}

		var numWeightedBackends, totalWeightSet int32
		for _, path := range paths {
			backendRef, err := toBackendRef(path.path.Backend)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			if path.extra != nil && path.extra.canary != nil && path.extra.canary.weight != 0 {
				weight := int32(path.extra.canary.weight)
				backendRef.Weight = &weight
				totalWeightSet += weight
				numWeightedBackends++
			}
			hrRule.BackendRefs = append(hrRule.BackendRefs, gatewayv1beta1.HTTPBackendRef{BackendRef: *backendRef})
		}
		if numWeightedBackends > 0 && numWeightedBackends < int32(len(hrRule.BackendRefs)) {
			weightToSet := (int32(100) - totalWeightSet) / (int32(len(hrRule.BackendRefs)) - numWeightedBackends)
			for i, br := range hrRule.BackendRefs {
				if br.Weight == nil {
					br.Weight = &weightToSet
					hrRule.BackendRefs[i] = br
				}
			}
		}
		httpRoute.Spec.Rules = append(httpRoute.Spec.Rules, hrRule)
	}

	return httpRoute, errors
}

func getPathMatchKey(ip ingressPath) pathMatchKey {
	var pathType string
	if ip.path.PathType != nil {
		pathType = string(*ip.path.PathType)
	}
	var canaryHeaderKey string
	if ip.extra != nil && ip.extra.canary != nil && ip.extra.canary.headerKey != "" {
		canaryHeaderKey = ip.extra.canary.headerKey
	}
	return pathMatchKey(fmt.Sprintf("%s/%s/%s", pathType, ip.path.Path, canaryHeaderKey))
}

func toHTTPRouteMatch(ip ingressPath) (*gatewayv1beta1.HTTPRouteMatch, error) {
	pmPrefix := gatewayv1beta1.PathMatchPathPrefix
	pmExact := gatewayv1beta1.PathMatchExact
	hmExact := gatewayv1beta1.HeaderMatchExact
	hmRegex := gatewayv1beta1.HeaderMatchRegularExpression

	match := &gatewayv1beta1.HTTPRouteMatch{Path: &gatewayv1beta1.HTTPPathMatch{Value: &ip.path.Path}}
	switch *ip.path.PathType {
	case networkingv1.PathTypePrefix:
		match.Path.Type = &pmPrefix
	case networkingv1.PathTypeExact:
		match.Path.Type = &pmExact
	default:
		return nil, fmt.Errorf("Unsupported path match type: %s", *ip.path.PathType)
	}

	if ip.extra != nil && ip.extra.canary != nil && ip.extra.canary.headerKey != "" {
		headerMatch := gatewayv1beta1.HTTPHeaderMatch{
			Name:  gatewayv1beta1.HTTPHeaderName(ip.extra.canary.headerKey),
			Value: ip.extra.canary.headerValue,
			Type:  &hmExact,
		}
		if ip.extra.canary.headerRegexMatch {
			headerMatch.Type = &hmRegex
		}
		match.Headers = []gatewayv1beta1.HTTPHeaderMatch{headerMatch}
	}

	return match, nil
}

func toBackendRef(ib networkingv1.IngressBackend) (*gatewayv1beta1.BackendRef, error) {
	if ib.Service != nil {
		if ib.Service.Port.Name != "" {
			return nil, fmt.Errorf("Named ports not supported: %s", ib.Service.Port.Name)
		}
		return &gatewayv1beta1.BackendRef{
			BackendObjectReference: gatewayv1beta1.BackendObjectReference{
				Name: gatewayv1beta1.ObjectName(ib.Service.Name),
				Port: (*gatewayv1beta1.PortNumber)(&ib.Service.Port.Number),
			},
		}, nil
	}
	return &gatewayv1beta1.BackendRef{
		BackendObjectReference: gatewayv1beta1.BackendObjectReference{
			Group: (*gatewayv1beta1.Group)(ib.Resource.APIGroup),
			Kind:  (*gatewayv1beta1.Kind)(&ib.Resource.Kind),
			Name:  gatewayv1beta1.ObjectName(ib.Resource.Name),
		},
	}, nil
}

func nameFromHost(host string) string {
	// replace all special chars with -
	reg, _ := regexp.Compile("[^a-zA-Z0-9]+")
	step1 := reg.ReplaceAllString(host, "-")
	// remove all - at start of string
	reg2, _ := regexp.Compile("^[^a-zA-Z0-9]+")
	step2 := reg2.ReplaceAllString(step1, "")
	// if nothing left, return "all-hosts"
	if len(host) == 0 {
		return "all-hosts"
	}
	return step2
}

func getExtra(ingress networkingv1.Ingress) *extra {
	e := &extra{}
	if c := ingress.Annotations["nginx.ingress.kubernetes.io/canary"]; c == "true" {
		e.canary = &canary{enable: true}
		if cHeader := ingress.Annotations["nginx.ingress.kubernetes.io/canary-by-header"]; cHeader != "" {
			e.canary.headerKey = cHeader
			e.canary.headerValue = "always"
		}
		if cHeaderVal := ingress.Annotations["nginx.ingress.kubernetes.io/canary-by-header-value"]; cHeaderVal != "" {
			e.canary.headerValue = cHeaderVal
		}
		if cHeaderRegex := ingress.Annotations["nginx.ingress.kubernetes.io/canary-by-header-pattern"]; cHeaderRegex != "" {
			e.canary.headerValue = cHeaderRegex
			e.canary.headerRegexMatch = true
		}
		if cHeaderWeight := ingress.Annotations["nginx.ingress.kubernetes.io/canary-weight"]; cHeaderWeight != "" {
			e.canary.weight, _ = strconv.Atoi(cHeaderWeight)
			e.canary.weightTotal = 100
		}
		if cHeaderWeightTotal := ingress.Annotations["nginx.ingress.kubernetes.io/canary-weight-total"]; cHeaderWeightTotal != "" {
			e.canary.weightTotal, _ = strconv.Atoi(cHeaderWeightTotal)
		}
	}
	return e
}
