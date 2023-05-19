/*
Copyright 2023 TV 2 DANMARK A/S

Licensed under the Apache License, Version 2.0 (the "License") with the
following modification to section 6. Trademarks:

Section 6. Trademarks is deleted and replaced by the following wording:

6. Trademarks. This License does not grant permission to use the trademarks and
trade names of TV 2 DANMARK A/S, including but not limited to the TV 2® logo and
word mark, except (a) as required for reasonable and customary use in describing
the origin of the Work, e.g. as described in section 4(c) of the License, and
(b) to reproduce the content of the NOTICE file. Any reference to the Licensor
must be made by making a reference to "TV 2 DANMARK A/S", written in capitalized
letters as in this example, unless the format in which the reference is made,
requires lower case letters.

You may not use this software except in compliance with the License and the
modifications set out above.

You may obtain a copy of the license at:

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	sigsyaml "sigs.k8s.io/yaml"
)

// Information about a resource, rendered format as well as actual in API server
type Composite struct {
	// The rendered resource
	Rendered *unstructured.Unstructured

	// GVR for resource
	GVR *schema.GroupVersionResource

	// Current resource fetch from API-server (or as close as our local caching allows)
	Current *unstructured.Unstructured

	// Whether resource is namespaced or not
	IsNamespaced bool
}

// Rendering and applying templates is a multi-stage process. This
// structure holds information about a rendered template between
// stages
type ResourceTemplateState struct {
	// Compiled template
	Template *template.Template

	// Resource information, rendered and current
	Resource Composite

	// Name of rendered resource (from template key in GatewayClassBlueprint, not Kubernetes resource name)
	TemplateName string

	// Raw template for resource
	StringTemplate string
}

// Parameters used when rendering templates
type TemplateValues struct {
	// Parent Gateway, always defined
	Gateway *map[string]any

	// Parent HTTPRoute. Only set when rendering HTTPRoute templates
	HTTPRoute map[string]any

	// Template values
	Values map[string]any

	// Current resources (i.e. sibling resources)
	Resources map[string]any

	// List of all hostnames across all listeners and attached
	// HTTPRoutes. These lists of hostnames are particularly
	// useful for TLS certificates which are not port specific.
	Hostnames TemplateHostnameValues
}

type TemplateHostnameValues struct {
	// Union and intersection of all hostnames across all
	// listeners and attached HTTPRoutes (with duplicates
	// removed). Intersection holds all hostnames from Union with
	// duplicates covered by wildcards removed.
	Union, Intersection []string
}

// Parse a single template with our additional functions added
func parseSingleTemplate(tmplKey, tmpl string) (*template.Template, error) {
	var funcs = template.FuncMap{
		"toYaml": helperToYaml,
	}
	return template.New(tmplKey).Option("missingkey=error").Funcs(sprig.TxtFuncMap()).Funcs(funcs).Parse(tmpl)
}

// Initialize ResourceTemplateState slice by parsing templates
func parseTemplates(resourceTemplates map[string]string) ([]*ResourceTemplateState, error) {
	var err error

	templates := make([]*ResourceTemplateState, 0, len(resourceTemplates))

	for tmplKey, tmpl := range resourceTemplates {
		r := ResourceTemplateState{}
		r.TemplateName = tmplKey
		r.StringTemplate = tmpl
		r.Template, err = parseSingleTemplate(tmplKey, tmpl)
		if err != nil {
			return nil, fmt.Errorf("cannot parse template %q: %w", tmplKey, err)
		}
		templates = append(templates, &r)
	}

	return templates, nil
}

// Attempt to render templates and get current resource, skipping
// resources that have already been rendered/fetched. Note that
// fetching current resource from API server/cache require that we can
// render the template first. Rendering errors on final attempt are
// logged as errors.
func renderTemplates(ctx context.Context, r ControllerDynClient, parent metav1.Object,
	templates []*ResourceTemplateState, values *TemplateValues, isFinalAttempt bool) (rendered, exists int) {
	var err error

	logger := log.FromContext(ctx)
	ns := parent.GetNamespace()

	for _, tmplRes := range templates {
		if tmplRes.Resource.Rendered == nil {
			tmplRes.Resource.Rendered, err = template2Unstructured(tmplRes.Template, values)
			if err != nil {
				if isFinalAttempt {
					logger.Error(err, "cannot render template", "templateName", tmplRes.TemplateName)
					// FIXME: These are convenient, but we should have a better logging design, i.e. it should be possible to enable rendering errors only
					fmt.Printf("Template:\n%s\n", tmplRes.StringTemplate)
					fmt.Printf("Template values:\n%+v\n", values)
				}
				continue
			}
		}
		if tmplRes.Resource.GVR == nil {
			tmplRes.Resource.GVR, tmplRes.Resource.IsNamespaced, err = unstructuredToGVR(r, tmplRes.Resource.Rendered)
			if err != nil {
				logger.Error(err, "cannot detect GVR for resource", "templateName", tmplRes.TemplateName)
				continue
			}
		}
		rendered++
		if tmplRes.Resource.Current == nil {
			var dynamicClient dynamic.ResourceInterface
			if tmplRes.Resource.IsNamespaced {
				dynamicClient = r.DynamicClient().Resource(*tmplRes.Resource.GVR).Namespace(ns)
			} else {
				dynamicClient = r.DynamicClient().Resource(*tmplRes.Resource.GVR)
			}
			tmplRes.Resource.Current, err = dynamicClient.Get(ctx, tmplRes.Resource.Rendered.GetName(), metav1.GetOptions{})
			if err != nil {
				logger.Error(err, "cannot get current resource", "templateName", tmplRes.TemplateName)
				continue
			}
			logger.Info("update current", "templatename", tmplRes.TemplateName, "current", tmplRes.Resource.Current)
		} else {
			logger.Info("already have update current", "templatename", tmplRes.TemplateName, "current", tmplRes.Resource.Current)
		}
		exists++
	}
	return rendered, exists
}

// Build a map of values from current resources. Useful for
// referencing values between resources, e.g. a status field from one
// resource may be used to template another resource
func buildResourceValues(templates []*ResourceTemplateState) map[string]any {
	resources := map[string]any{}

	for _, tmplRes := range templates {
		if tmplRes.Resource.Current != nil {
			resources[tmplRes.TemplateName] = tmplRes.Resource.Current.UnstructuredContent()
		}
	}
	return resources
}

// Apply a list of pre-rendered templates and set owner reference for
// namespaced resources
func applyTemplates(ctx context.Context, r ControllerDynClient, parent metav1.Object, templates []*ResourceTemplateState) error {
	var err error
	var errorCnt = 0

	logger := log.FromContext(ctx)

	for _, tmplRes := range templates {
		if tmplRes.Resource.Rendered == nil || tmplRes.Resource.GVR == nil {
			// We do not yet have enough information to render/apply this resource
			continue
		}
		if tmplRes.Resource.IsNamespaced {
			// Only namespaced objects can have namespaced object as owner
			err = ctrl.SetControllerReference(parent, tmplRes.Resource.Rendered, r.Scheme())
			if err != nil {
				logger.Error(err, "cannot set owner for namespaced template", "templateName", tmplRes.TemplateName)
				errorCnt++
			} else {
				ns := parent.GetNamespace()
				err = patchUnstructured(ctx, r, tmplRes.Resource.Rendered, tmplRes.Resource.GVR, &ns)
				if err != nil {
					logger.Error(err, "cannot apply namespaced template", "templateName", tmplRes.TemplateName)
					errorCnt++
				}
			}
		} else {
			err = patchUnstructured(ctx, r, tmplRes.Resource.Rendered, tmplRes.Resource.GVR, nil)
			if err != nil {
				logger.Error(err, "cannot apply cluster-scoped template", "templateName", tmplRes.TemplateName)
				errorCnt++
			}
		}
	}

	if errorCnt > 0 {
		return fmt.Errorf("found %v problems while applying %v templates", errorCnt, len(templates))
	}
	return nil
}

// This function is made available to templates as 'toYaml'
func helperToYaml(v interface{}) string {
	data, err := sigsyaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(data), "\n")
}

func templateRender(tmpl *template.Template, templateValues *TemplateValues) (*bytes.Buffer, error) {
	var buffer bytes.Buffer

	if err := tmpl.Execute(io.Writer(&buffer), templateValues); err != nil {
		return nil, err
	}

	// FIXME: These are convenient, but we should have a better logging design, i.e. it should be possible to enable rendering info only
	fmt.Printf("Rendered:\n%s\n", buffer.Bytes())
	fmt.Printf("Values:\n%+v\n", templateValues)

	return &buffer, nil
}

func template2map(tmpl *template.Template, tmplValues *TemplateValues) (map[string]any, error) {
	renderBuffer, err := templateRender(tmpl, tmplValues)
	if err != nil {
		return nil, err
	}

	rawResource := map[string]any{}
	err = yaml.Unmarshal(renderBuffer.Bytes(), &rawResource)
	if err != nil {
		return nil, err
	}
	return rawResource, nil
}

func template2Unstructured(tmpl *template.Template, tmplValues *TemplateValues) (*unstructured.Unstructured, error) {
	rawResource, err := template2map(tmpl, tmplValues)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: rawResource}, nil
}

// Prepare a resource like Gateway or HTTPRoute for use in templates
// by converting to map[string]any
func objectToMap(obj runtime.Object) (map[string]any, error) {
	mapObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("cannot convert %+v: %w", obj, err)
	}
	return mapObj, nil
}
