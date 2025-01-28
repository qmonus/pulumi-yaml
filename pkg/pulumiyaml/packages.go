// Copyright 2022, Pulumi Corporation.  All rights reserved.

package pulumiyaml

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/blang/semver"
	"github.com/iancoleman/strcase"
	"github.com/pulumi/pulumi-yaml/pkg/pulumiyaml/ast"
	"github.com/pulumi/pulumi-yaml/pkg/pulumiyaml/packages"
	"github.com/pulumi/pulumi-yaml/pkg/pulumiyaml/syntax"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

type ResourceTypeToken string

func (rtt ResourceTypeToken) String() string {
	return string(rtt)
}

type FunctionTypeToken string

func (ftt FunctionTypeToken) String() string {
	return string(ftt)
}

// Package is our external facing term, e.g.: a provider package in the registry. Packages are
// delivered via plugins, and this interface provides enough surface area to get information about
// resources in a package.
type Package interface {
	// Returns the name of the package.
	Name() string
	// Returns the version of the package.
	Version() *semver.Version
	// Given a type name, look up that type in a package's defined resources and return a canonical
	// type name. The lookup may take the form of trying alternate names or aliases.
	//
	// e.g.: given "aws:s3:Bucket", it will return "aws:s3/bucket:Bucket".
	ResolveResource(typeName string) (ResourceTypeToken, error)
	// Given a type name, look up that type in a package's defined resources and return a canonical
	// type name. The lookup may take the form of trying alternate names or aliases.
	//
	// e.g.: given "aws:s3:Bucket", it will return "aws:s3/bucket:Bucket".
	ResolveFunction(typeName string) (FunctionTypeToken, error)
	// Given the canonical name of a resource, return the IsComponent property of the resource schema.
	IsComponent(typeName ResourceTypeToken) (bool, error)
	// Given the canonical name of a resource, and a property name, return if that property should be secret.
	IsResourcePropertySecret(typeName ResourceTypeToken, propertyName string) (bool, error)
	// Information on the properties of a resource. All resource type tokens generated by a
	// package must return a non-nil instance of `TypeHint` when called.
	ResourceTypeHint(typeName ResourceTypeToken) *schema.ResourceType
	// Information on the argument to a function.
	FunctionTypeHint(typeName FunctionTypeToken) *schema.Function
	// Gets properties with constant values that must be added to the register resource API call.
	ResourceConstants(typeName ResourceTypeToken) map[string]interface{}
}

type PackageLoader interface {
	LoadPackage(ctx context.Context, descriptor *schema.PackageDescriptor) (Package, error)
	Close()
}

type packageLoader struct {
	schema.ReferenceLoader

	host plugin.Host
}

func (l packageLoader) LoadPackage(ctx context.Context, descriptor *schema.PackageDescriptor) (Package, error) {
	pkg, err := l.ReferenceLoader.LoadPackageReferenceV2(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	return resourcePackage{pkg}, nil
}

func (l packageLoader) Close() {
	if l.host != nil {
		l.host.Close()
	}
}

func NewPackageLoader(plugins *workspace.Plugins) (PackageLoader, error) {
	host, err := newResourcePackageHost(plugins)
	if err != nil {
		return nil, err
	}
	return packageLoader{schema.NewPluginLoader(host), host}, nil
}

// Unsafely create a PackageLoader from a schema.Loader, forfeiting the ability to close the host
// and clean up plugins when finished. Useful for test cases.
func NewPackageLoaderFromSchemaLoader(loader schema.ReferenceLoader) PackageLoader {
	return packageLoader{loader, nil}
}

// GetReferencedPackages returns the packages and (if provided) versions for each referenced package
// used in the program.
func GetReferencedPackages(tmpl *ast.TemplateDecl) ([]packages.PackageDecl, syntax.Diagnostics) {
	packageMap := map[string]*packages.PackageDecl{}

	// Iterate over the package declarations
	for _, pkg := range tmpl.Packages {
		pkg := pkg
		name := pkg.Name
		version := pkg.Version
		if pkg.Parameterization != nil {
			name = pkg.Parameterization.Name
			version = pkg.Parameterization.Version
		}

		if entry, found := packageMap[name]; found {
			if entry.Version == "" {
				entry.Version = version
			}
			if entry.DownloadURL == "" {
				entry.DownloadURL = pkg.DownloadURL
			}
		} else {
			packageMap[name] = &pkg
		}
	}

	acceptType := func(r *Runner, typeName string, version, pluginDownloadURL *ast.StringExpr) {
		pkg := ResolvePkgName(typeName)
		if entry, found := packageMap[pkg]; found {
			if v := version.GetValue(); v != "" && entry.Version != v {
				if entry.Version == "" {
					entry.Version = v
				} else {
					r.sdiags.Extend(ast.ExprError(version, fmt.Sprintf("Package %v already declared with a conflicting version: %v", pkg, entry.Version), ""))
				}
			}
			if url := pluginDownloadURL.GetValue(); url != "" && entry.DownloadURL != url {
				if entry.DownloadURL == "" {
					entry.DownloadURL = url
				} else {
					r.sdiags.Extend(ast.ExprError(pluginDownloadURL, fmt.Sprintf("Package %v already declared with a conflicting plugin download URL: %v", pkg, entry.DownloadURL), ""))
				}
			}
		} else {
			packageMap[pkg] = &packages.PackageDecl{
				Name:        pkg,
				Version:     version.GetValue(),
				DownloadURL: pluginDownloadURL.GetValue(),
			}
		}
	}

	diags := newRunner(tmpl, nil).Run(walker{
		VisitResource: func(r *Runner, node resourceNode) bool {
			res := node.Value

			if res.Type == nil {
				r.sdiags.Extend(syntax.NodeError(node.Value.Syntax(), fmt.Sprintf("Resource declared without a 'type': %q", node.Key.Value), ""))
				return true
			}
			acceptType(r, res.Type.Value, res.Options.Version, res.Options.PluginDownloadURL)

			return true
		},
		VisitExpr: func(ctx *evalContext, expr ast.Expr) bool {
			if expr, ok := expr.(*ast.InvokeExpr); ok {
				if expr.Token == nil {
					ctx.Runner.sdiags.Extend(syntax.NodeError(expr.Syntax(), "Invoke declared without a 'function' type", ""))
					return true
				}
				acceptType(ctx.Runner, expr.Token.GetValue(), expr.CallOpts.Version, expr.CallOpts.PluginDownloadURL)
			}
			return true
		},
	})

	if diags.HasErrors() {
		return nil, diags
	}

	var packages []packages.PackageDecl
	for _, pkg := range packageMap {
		// Skip the built-in pulumi package
		if pkg.Name == "pulumi" {
			continue
		}
		packages = append(packages, *pkg)
	}

	sort.Slice(packages, func(i, j int) bool {
		pI, pJ := packages[i], packages[j]
		if pI.Name != pJ.Name {
			return pI.Name < pJ.Name
		}
		if pI.Version != pJ.Version {
			return pI.Version < pJ.Version
		}
		if pI.Parameterization == nil && pJ.Parameterization == nil {
			return pI.DownloadURL < pJ.DownloadURL
		}
		if pI.Parameterization == nil {
			return true
		}
		if pJ.Parameterization == nil {
			return false
		}
		if pI.Parameterization.Name != pJ.Parameterization.Name {
			return pI.Parameterization.Name < pJ.Parameterization.Name
		}
		return pI.Parameterization.Version < pJ.Parameterization.Version
	})

	return packages, nil
}

func ResolvePkgName(typeString string) string {
	typeParts := strings.Split(typeString, ":")

	// If it's pulumi:providers:aws, the package name is the last label:
	if len(typeParts) == 3 && typeParts[0] == "pulumi" && typeParts[1] == "providers" {
		return typeParts[2]
	}

	return typeParts[0]
}

func loadPackage(
	ctx context.Context, loader PackageLoader,
	descriptors map[tokens.Package]*schema.PackageDescriptor, typeString string, version *semver.Version,
) (Package, error) {
	typeParts := strings.Split(typeString, ":")
	if len(typeParts) < 2 || len(typeParts) > 3 {
		return nil, fmt.Errorf("invalid type token %q", typeString)
	}

	packageName := ResolvePkgName(typeString)
	descriptor := descriptors[tokens.Package(packageName)]
	if descriptor == nil {
		// Fall back to just the package name and passed in version if we don't have a descriptor.
		descriptor = &schema.PackageDescriptor{
			Name:    packageName,
			Version: version,
		}
	}
	if version != nil {
		// Override the version if one was passed in.
		descriptor.Version = version
	}

	pkg, err := loader.LoadPackage(ctx, descriptor)
	if errors.Is(err, schema.ErrGetSchemaNotImplemented) {
		return nil, fmt.Errorf("error loading schema for %q: %w", packageName, err)
	} else if err != nil {
		return nil, fmt.Errorf("internal error loading package %q: %w", packageName, err)
	}

	return pkg, nil
}

// Unavailable in Docker versions <4.
var docker3ResourceNames = map[string]struct{}{
	"docker:image:Image": {},
	"docker:Image":       {},
}

var kubernetesResourceNames = map[string]string{
	// Prevent errors with custom resource types that are not supported in YAML by commenting them out.
	// JIRA: https://m-pipe.atlassian.net/browse/IACS-334
	// "kubernetes:apiextensions.k8s.io:CustomResource": "https://github.com/pulumi/pulumi-kubernetes/issues/1971",
	"kubernetes:kustomize:Directory": "https://github.com/pulumi/pulumi-kubernetes/issues/1971",
	"kubernetes:yaml:ConfigFile":     "https://github.com/pulumi/pulumi-kubernetes/issues/1971",
	"kubernetes:yaml:ConfigGroup":    "https://github.com/pulumi/pulumi-kubernetes/issues/1971",
}

var helmResourceNames = map[string]struct{}{
	"kubernetes:helm.sh/v2:Chart": {},
	"kubernetes:helm.sh/v3:Chart": {},
}

// ResolveResource determines the appropriate package for a resource, loads that package, then calls
// the package's ResolveResource method to determine the canonical name of the resource, returning
// both the package and the canonical name.
func ResolveResource(ctx context.Context, loader PackageLoader,
	descriptors map[tokens.Package]*schema.PackageDescriptor,
	typeString string, version *semver.Version) (Package, ResourceTypeToken, error) {
	if issue, found := kubernetesResourceNames[typeString]; found {
		return nil, "", fmt.Errorf("The resource type [%v] is not supported in YAML at this time, see: %v", typeString, issue)
	}

	if _, found := helmResourceNames[typeString]; found {
		return nil, "", fmt.Errorf("Helm Chart resources are not supported in YAML, consider using the Helm Release resource instead: https://www.pulumi.com/registry/packages/kubernetes/api-docs/helm/v3/release/")
	}

	pkg, err := loadPackage(ctx, loader, descriptors, typeString, version)
	if err != nil {
		return nil, "", err
	}

	if _, found := docker3ResourceNames[typeString]; found {
		// To avoid requiring the user to manually specify the version to use, we check if
		// the *resolved* pkg version is greater then 4.*.
		if v := pkg.Version(); v == nil || v.Major <= 3 {
			contract.Assertf(version == nil || version.Major <= 3, "make sure we have not requested an appropriately versioned package")
			return nil, "", fmt.Errorf("Docker Image resources are not supported in YAML without major version >= 4, see: https://github.com/pulumi/pulumi-yaml/issues/421")
		}
	}

	canonicalName, err := pkg.ResolveResource(typeString)
	if err != nil {
		return nil, "", err
	}

	return pkg, canonicalName, nil
}

// ResolveFunction determines the appropriate package for a function, loads that package, then calls
// the package's ResolveFunction method to determine the canonical name of the function, returning
// both the package and the canonical name.
func ResolveFunction(ctx context.Context, loader PackageLoader,
	descriptors map[tokens.Package]*schema.PackageDescriptor,
	typeString string, version *semver.Version) (Package, FunctionTypeToken, error) {
	pkg, err := loadPackage(ctx, loader, descriptors, typeString, version)
	if err != nil {
		return nil, "", err
	}
	canonicalName, err := pkg.ResolveFunction(typeString)
	if err != nil {
		return nil, "", err
	}

	return pkg, canonicalName, nil
}

type resourcePackage struct {
	schema.PackageReference
}

func NewResourcePackage(pkg schema.PackageReference) Package {
	return resourcePackage{pkg}
}

func (p resourcePackage) resolveProvider(typeName string) (ResourceTypeToken, bool) {
	typeParts := strings.Split(typeName, ":")
	// pulumi:providers:$pkgName
	if len(typeParts) == 3 &&
		typeParts[0] == "pulumi" &&
		typeParts[1] == "providers" &&
		typeParts[2] == p.Name() {
		return ResourceTypeToken(typeName), true
	}
	return "", false
}

func resolveToken(typeName string, resolve func(string) (string, bool, error)) (string, bool, error) {
	typeParts := strings.Split(typeName, ":")
	if len(typeParts) < 2 || len(typeParts) > 3 {
		return "", false, fmt.Errorf("invalid type token %q", typeName)
	}

	if token, found, err := resolve(typeName); found {
		return token, true, nil
	} else if err != nil {
		return "", false, err
	}

	// If the provided type token is `$pkg:type`, expand it to `$pkg:index:type` automatically. We
	// may well want to handle this more fundamentally in Pulumi itself to avoid the need for
	// `:index:` ceremony quite generally.
	if len(typeParts) == 2 {
		alternateName := fmt.Sprintf("%s:index:%s", typeParts[0], typeParts[1])
		if token, found, err := resolve(alternateName); found {
			return token, true, nil
		} else if err != nil {
			return "", false, err
		}
		typeParts = []string{typeParts[0], "index", typeParts[1]}
	}

	// A legacy of classic providers is resources with names like `aws:s3/bucket:Bucket`. Here, we
	// allow the user to enter `aws:s3:Bucket`, and we interpolate in the 3rd label, camel cased.
	if len(typeParts) == 3 {
		repeatedSection := strcase.ToLowerCamel(typeParts[2])
		alternateName := fmt.Sprintf("%s:%s/%s:%s", typeParts[0], typeParts[1], repeatedSection, typeParts[2])
		if token, found, err := resolve(alternateName); found {
			return token, true, nil
		} else if err != nil {
			return "", false, err
		}
	}

	return "", false, nil
}

func (p resourcePackage) ResolveResource(typeName string) (ResourceTypeToken, error) {
	if tk, ok := p.resolveProvider(typeName); ok {
		return tk, nil
	}

	tk, ok, err := resolveToken(typeName, func(tk string) (string, bool, error) {
		if res, found, err := p.Resources().Get(tk); found {
			return res.Token, true, nil
		} else if err != nil {
			return "", false, err
		}
		return "", false, nil
	})

	if err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("unable to find resource type %q in resource provider %q", typeName, p.Name())
	}

	return ResourceTypeToken(tk), nil
}

func (p resourcePackage) ResolveFunction(typeName string) (FunctionTypeToken, error) {
	typeParts := strings.Split(typeName, ":")
	if len(typeParts) < 2 || len(typeParts) > 3 {
		return "", fmt.Errorf("invalid type token %q", typeName)
	}

	tk, ok, err := resolveToken(typeName, func(tk string) (string, bool, error) {
		if fn, found, err := p.Functions().Get(tk); found {
			return fn.Token, true, nil
		} else if err != nil {
			return "", false, err
		}
		return "", false, nil
	})

	if err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("unable to find function %q in resource provider %q", typeName, p.Name())
	}

	return FunctionTypeToken(tk), nil
}

func (p resourcePackage) IsComponent(typeName ResourceTypeToken) (bool, error) {
	if res, found, err := p.Resources().Get(string(typeName)); found {
		return res.IsComponent, nil
	} else if err != nil {
		return false, err
	}
	return false, fmt.Errorf("unable to find resource type %q in resource provider %q", typeName, p.Name())
}

func (p resourcePackage) IsResourcePropertySecret(typeName ResourceTypeToken, propertyName string) (bool, error) {
	if res, found, err := p.Resources().Get(string(typeName)); found {
		for _, prop := range res.InputProperties {
			if prop.Name == propertyName {
				return prop.Secret, nil
			}
		}
		return false, fmt.Errorf(
			"unable to find property %q on resource %q in resource provider %q",
			propertyName, typeName, p.Name())
	} else if err != nil {
		return false, err
	}
	return false, fmt.Errorf("unable to find resource type %q in resource provider %q", typeName, p.Name())
}

func (p resourcePackage) Name() string {
	return p.PackageReference.Name()
}

func (p resourcePackage) ResourceTypeHint(typeName ResourceTypeToken) *schema.ResourceType {
	if _, ok := p.resolveProvider(typeName.String()); ok {
		prov, err := p.Provider()
		if err != nil {
			return nil
		}
		return &schema.ResourceType{
			Token:    typeName.String(),
			Resource: prov,
		}
	}
	r, ok, err := p.Resources().Get(typeName.String())
	if !ok || err != nil {
		return nil
	}
	return &schema.ResourceType{
		Token:    typeName.String(),
		Resource: r,
	}
}

func (p resourcePackage) FunctionTypeHint(typeName FunctionTypeToken) *schema.Function {
	f, ok, err := p.Functions().Get(typeName.String())
	if !ok || err != nil {
		return nil
	}
	return f
}

func (p resourcePackage) ResourceConstants(typeName ResourceTypeToken) map[string]interface{} {
	_, ok := p.resolveProvider(typeName.String())
	if ok {
		prov, err := p.Provider()
		if err != nil {
			return nil
		}
		return getResourceConstants(prov.Properties)
	}
	res, ok, _ := p.Resources().Get(typeName.String())
	if ok {
		return getResourceConstants(res.Properties)
	}
	return nil
}

func getResourceConstants(props []*schema.Property) map[string]interface{} {
	constantProps := map[string]interface{}{}
	for _, v := range props {
		if v.ConstValue != nil {
			constantProps[v.Name] = v.ConstValue
		}
	}

	return constantProps
}

func newResourcePackageHost(plugins *workspace.Plugins) (plugin.Host, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	sink := diag.DefaultSink(os.Stderr, os.Stderr, diag.FormatOptions{
		Color: cmdutil.GetGlobalColorization(),
	})
	pluginCtx, err := plugin.NewContextWithRoot(sink, sink, nil, cwd, cwd, nil, true, nil, plugins, nil, nil)
	if err != nil {
		return nil, err
	}

	return pluginCtx.Host, nil
}
